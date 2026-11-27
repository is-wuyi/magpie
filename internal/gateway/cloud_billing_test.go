package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/cloud/pricing"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func httptestRequestWithIdentity(who access.Identity, state *cloudBillingState) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx := access.WithIdentity(r.Context(), who)
	return r.WithContext(context.WithValue(ctx, cloudBillingContextKey{}, state))
}

type fakeCloudPrices struct{ err error }

func (f fakeCloudPrices) Quote(_ context.Context, channel, provider, model string) (pricing.Quote, error) {
	if f.err != nil {
		return pricing.Quote{}, f.err
	}
	return pricing.NewQuote(pricing.Key{Channel: channel, Provider: provider, Model: model}, pricing.Price{InputPerMTok: 10, OutputPerMTok: 20})
}

type fakeCloudBilling struct {
	reserve []wallet.ReserveRequest
	settle  []wallet.SettleRequest
	refund  []wallet.RefundRequest
}

func (f *fakeCloudBilling) Reserve(_ context.Context, req wallet.ReserveRequest) (wallet.Reservation, error) {
	f.reserve = append(f.reserve, req)
	snapshot, err := pricing.NewSnapshot(req.Quote, time.Now())
	if err != nil {
		return wallet.Reservation{}, err
	}
	return wallet.Reservation{ID: uuid.New(), UserID: req.UserID, RequestID: req.RequestID, Status: wallet.StatusHeld, Snapshot: snapshot, HoldMicros: 100}, nil
}
func (f *fakeCloudBilling) Settle(_ context.Context, req wallet.SettleRequest) (wallet.Settlement, error) {
	f.settle = append(f.settle, req)
	return wallet.Settlement{ReservationID: req.ReservationID}, nil
}
func (f *fakeCloudBilling) Refund(_ context.Context, req wallet.RefundRequest) (wallet.RefundResult, error) {
	f.refund = append(f.refund, req)
	return wallet.RefundResult{ReservationID: req.ReservationID}, nil
}

func TestCloudAttemptAggregatesUsageAndSettlesOnce(t *testing.T) {
	billing := &fakeCloudBilling{}
	state := &cloudBillingState{billing: billing, prices: fakeCloudPrices{}}
	who := access.Identity{Cloud: true, UserID: uuid.NewString(), KeyID: "key"}
	r := httptestRequestWithIdentity(who, state)
	p := provider.Provider{ID: "h-channel-openai", HouseChannelID: uuid.NewString()}
	if err := reserveCloudAttempt(r, p, "gpt-test", []byte(`{"max_tokens":100}`)); err != nil {
		t.Fatal(err)
	}
	if len(billing.reserve) != 1 || billing.reserve[0].Quote.Key.Provider != p.ID || billing.reserve[0].EstimatedUsage.OutputTokens != 100 {
		t.Fatalf("reserve %+v", billing.reserve)
	}
	for _, rec := range []usage.Record{{Status: 200, Input: 10, Output: 5}, {Status: 200, Input: 2, Output: 3}} {
		addCloudUsage(r, &rec)
		if rec.CloudUserID != who.UserID || rec.HouseChannelID != p.HouseChannelID || rec.BillingReservationID == "" {
			t.Fatalf("unattributed usage %+v", rec)
		}
	}
	state.finish()
	if len(billing.settle) != 1 || len(billing.refund) != 0 {
		t.Fatalf("settle %v refund %v", billing.settle, billing.refund)
	}
	got := billing.settle[0].ActualUsage
	if got.InputTokens != 12 || got.OutputTokens != 8 {
		t.Fatalf("settled usage %+v", got)
	}
}

func TestCloudAttemptWithoutBillableUsageRefunds(t *testing.T) {
	billing := &fakeCloudBilling{}
	state := &cloudBillingState{billing: billing, prices: fakeCloudPrices{}}
	r := httptestRequestWithIdentity(access.Identity{Cloud: true, UserID: uuid.NewString()}, state)
	p := provider.Provider{ID: "h-channel-openai", HouseChannelID: uuid.NewString()}
	if err := reserveCloudAttempt(r, p, "gpt-test", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	addCloudUsage(r, &usage.Record{Status: 500})
	state.finish()
	if len(billing.refund) != 1 || len(billing.settle) != 0 {
		t.Fatalf("settle %v refund %v", billing.settle, billing.refund)
	}
}

func TestCloudAttemptRefusesAnotherTenantEvenBeforeBYOK(t *testing.T) {
	cloud.SetEnabled(true)
	t.Cleanup(func() { cloud.SetEnabled(false) })
	state := &cloudBillingState{billing: &fakeCloudBilling{}, prices: fakeCloudPrices{}}
	r := httptestRequestWithIdentity(access.Identity{Cloud: true, UserID: "user-a"}, state)
	err := reserveCloudAttempt(r, provider.Provider{ID: "u-user-b-openai", OwnerUserID: "user-b"}, "gpt-test", []byte(`{}`))
	if err == nil {
		t.Fatal("another tenant's BYOK provider passed the billing guard")
	}
}

func TestCloudBYOKNeverUsesHouseWallet(t *testing.T) {
	state := &cloudBillingState{billing: &fakeCloudBilling{}, prices: fakeCloudPrices{}}
	r := httptestRequestWithIdentity(access.Identity{Cloud: true, UserID: uuid.NewString()}, state)
	if err := reserveCloudAttempt(r, provider.Provider{ID: "u-user-openai", OwnerUserID: "user"}, "gpt-test", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	state.finish()
	if len(state.attempts) != 0 {
		t.Fatal("BYOK created a wallet reservation")
	}
}
