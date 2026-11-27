package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/pricing"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/usage"
)

type cloudUsageSet struct {
	usage  pricing.Usage
	status int
}

type cloudHoldWallet interface {
	StaleReservations(context.Context, time.Time, int) ([]wallet.Reservation, error)
	Settle(context.Context, wallet.SettleRequest) (wallet.Settlement, error)
	Refund(context.Context, wallet.RefundRequest) (wallet.RefundResult, error)
}

// reconcileCloudHolds settles expired holds from their diagnostic usage rows
// before refunding an attempt with no billable usage. An unreadable or absent
// log, or no row for a hold, is unknown: that hold is left for the next pass
// rather than refunded blindly.
func (r *cloudRuntime) reconcileCloudHolds(ctx context.Context) error {
	return reconcileCloudHolds(ctx, r.wallet, usage.Path(), time.Now())
}

func reconcileCloudHolds(ctx context.Context, w cloudHoldWallet, usagePath string, now time.Time) error {
	stale, err := w.StaleReservations(ctx, now, 100)
	if err != nil || len(stale) == 0 {
		return err
	}
	wanted := map[uuid.UUID]bool{}
	for _, reservation := range stale {
		wanted[reservation.ID] = true
	}
	records, err := cloudUsageByReservation(usagePath, wanted)
	if err != nil {
		return err
	}
	for _, reservation := range stale {
		record, found := records[reservation.ID]
		if !found {
			continue
		}
		total := record.usage.InputTokens + record.usage.OutputTokens + record.usage.CacheReadTokens + record.usage.CacheWriteTokens
		if record.status >= 400 && total == 0 {
			_, err = w.Refund(ctx, wallet.RefundRequest{ReservationID: reservation.ID, Reason: "stale hold with no billable usage"})
		} else {
			_, err = w.Settle(ctx, wallet.SettleRequest{ReservationID: reservation.ID, ActualUsage: record.usage, Description: reservation.Snapshot.Key.String() + " (recovered)"})
		}
		if err != nil && !errors.Is(err, wallet.ErrReservationSettled) && !errors.Is(err, wallet.ErrReservationRefunded) {
			return fmt.Errorf("reconcile reservation %s: %w", reservation.ID, err)
		}
	}
	return nil
}

func cloudUsageByReservation(path string, wanted map[uuid.UUID]bool) (map[uuid.UUID]cloudUsageSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[uuid.UUID]cloudUsageSet{}
	dec := json.NewDecoder(f)
	for dec.More() {
		var record usage.Record
		if err := dec.Decode(&record); err != nil {
			return nil, err
		}
		if record.BillingReservationID == "" {
			continue
		}
		id, err := uuid.Parse(record.BillingReservationID)
		if err != nil || !wanted[id] {
			continue
		}
		set := out[id]
		set.usage.InputTokens += int64(record.Input)
		set.usage.OutputTokens += int64(record.Output)
		set.usage.CacheReadTokens += int64(record.CacheRead)
		set.usage.CacheWriteTokens += int64(record.CacheWrite)
		set.status = record.Status
		out[id] = set
	}
	return out, nil
}
