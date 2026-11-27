package gateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/cloud/pricing"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// CloudBilling is the wallet's reservation lifecycle. Keeping the interface
// here lets gateway tests prove the request path without a database.
type CloudBilling interface {
	Reserve(context.Context, wallet.ReserveRequest) (wallet.Reservation, error)
	Settle(context.Context, wallet.SettleRequest) (wallet.Settlement, error)
	Refund(context.Context, wallet.RefundRequest) (wallet.RefundResult, error)
}

type CloudPrices interface {
	Quote(context.Context, string, string, string) (pricing.Quote, error)
}

var cloudBillingServices struct {
	sync.RWMutex
	billing CloudBilling
	prices  CloudPrices
}

func RegisterCloudBilling(b CloudBilling, p CloudPrices) {
	cloudBillingServices.Lock()
	defer cloudBillingServices.Unlock()
	if b == nil || p == nil {
		panic("gateway: incomplete cloud billing services")
	}
	if cloudBillingServices.billing != nil {
		panic("gateway: cloud billing services registered twice")
	}
	cloudBillingServices.billing, cloudBillingServices.prices = b, p
}

func currentCloudBilling() (CloudBilling, CloudPrices) {
	cloudBillingServices.RLock()
	defer cloudBillingServices.RUnlock()
	return cloudBillingServices.billing, cloudBillingServices.prices
}

type cloudBillingContextKey struct{}

type cloudAttempt struct {
	reservation wallet.Reservation
	usage       pricing.Usage
	status      int
	recorded    bool
}

type cloudBillingState struct {
	billing  CloudBilling
	prices   CloudPrices
	mu       sync.Mutex
	attempts []*cloudAttempt
	current  *cloudAttempt
}

func cloudRequestGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cloud.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		b, p := currentCloudBilling()
		state := &cloudBillingState{billing: b, prices: p}
		r = r.WithContext(context.WithValue(r.Context(), cloudBillingContextKey{}, state))
		defer state.finish()
		next.ServeHTTP(w, r)
	})
}

func cloudBillingOf(ctx context.Context) *cloudBillingState {
	s, _ := ctx.Value(cloudBillingContextKey{}).(*cloudBillingState)
	return s
}

var errCloudBillingUnavailable = errors.New("house-channel billing is unavailable")

// reserveCloudAttempt runs immediately before an upstream attempt. BYOK
// providers never reserve a house wallet. An unknown price or unavailable
// wallet fails closed before any upstream request is made.
func reserveCloudAttempt(r *http.Request, p provider.Provider, model string, body []byte) error {
	state := cloudBillingOf(r.Context())
	who := access.Caller(r.Context())
	if cloud.Enabled() && !providerAllowed(who, p) {
		return errors.New("this API key may not use " + p.ID)
	}
	if p.HouseChannelID == "" {
		return nil
	}
	if state == nil {
		return errCloudBillingUnavailable
	}
	userID, err := uuid.Parse(who.UserID)
	if !who.Cloud || err != nil || state.billing == nil || state.prices == nil {
		return errCloudBillingUnavailable
	}
	quote, err := state.prices.Quote(r.Context(), p.HouseChannelID, p.ID, model)
	if err != nil {
		return err
	}
	output := gjson.GetBytes(body, "max_output_tokens").Int()
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if output == 0 {
			output = gjson.GetBytes(body, key).Int()
		}
	}
	if output <= 0 {
		output = 4096
	}
	estimated := pricing.Usage{InputTokens: int64(len(body)) / 4, OutputTokens: output}
	reservation, err := state.billing.Reserve(r.Context(), wallet.ReserveRequest{
		UserID: userID, RequestID: uuid.NewString(), Quote: quote, EstimatedUsage: estimated, TTL: 24 * time.Hour,
		Description: p.HouseChannelID + "/" + p.ID + "/" + model,
	})
	if err != nil {
		return err
	}
	state.mu.Lock()
	state.attempts = append(state.attempts, &cloudAttempt{reservation: reservation})
	state.current = state.attempts[len(state.attempts)-1]
	state.mu.Unlock()
	return nil
}

// addCloudUsage attributes both the diagnostic usage row and the eventual
// settlement to the current attempt. Advisor rows follow the same current
// attempt and are aggregated before Settle runs once.
func addCloudUsage(r *http.Request, rec *usage.Record) {
	state := cloudBillingOf(r.Context())
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	a := state.current
	if a == nil {
		return
	}
	rec.CloudUserID = a.reservation.UserID.String()
	rec.HouseChannelID = a.reservation.Snapshot.Key.Channel
	rec.BillingReservationID = a.reservation.ID.String()
	a.usage.InputTokens += int64(rec.Input)
	a.usage.OutputTokens += int64(rec.Output)
	a.usage.CacheReadTokens += int64(rec.CacheRead)
	a.usage.CacheWriteTokens += int64(rec.CacheWrite)
	a.status = rec.Status
	a.recorded = true
}

func (s *cloudBillingState) finish() {
	s.mu.Lock()
	attempts := append([]*cloudAttempt(nil), s.attempts...)
	s.mu.Unlock()
	if len(attempts) == 0 {
		return
	}
	// Response cancellation must not lose the settlement. Bound the work,
	// however, so a broken wallet cannot pin a finished request forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, a := range attempts {
		if !a.recorded || a.status >= 400 && a.usage.InputTokens+a.usage.OutputTokens+a.usage.CacheReadTokens+a.usage.CacheWriteTokens == 0 {
			_, err := s.billing.Refund(ctx, wallet.RefundRequest{ReservationID: a.reservation.ID, Reason: "no billable upstream usage"})
			if err != nil {
				log.Printf("cloud billing: refund reservation %s: %v", a.reservation.ID, err)
			}
			continue
		}
		_, err := s.billing.Settle(ctx, wallet.SettleRequest{ReservationID: a.reservation.ID, ActualUsage: a.usage, Description: a.reservation.Snapshot.Key.String()})
		if err != nil {
			// The durable hold remains for reconciliation; it is never treated
			// as a successful settlement or silently charged a second time.
			log.Printf("cloud billing: settle reservation %s: %v", a.reservation.ID, err)
		}
	}
}
