package gui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/usage"
)

func TestCloudUsageByReservationAggregatesOnlyWantedRows(t *testing.T) {
	id, other := uuid.New(), uuid.New()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	rows := []usage.Record{{BillingReservationID: id.String(), Status: 200, Input: 10, Output: 4}, {BillingReservationID: id.String(), Status: 200, Input: 2, Output: 3, CacheRead: 5}, {BillingReservationID: other.String(), Status: 200, Input: 100}}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := cloudUsageByReservation(path, map[uuid.UUID]bool{id: true})
	if err != nil {
		t.Fatal(err)
	}
	set := got[id]
	if set.usage.InputTokens != 12 || set.usage.OutputTokens != 7 || set.usage.CacheReadTokens != 5 {
		t.Fatalf("%+v", set)
	}
	if _, found := got[other]; found {
		t.Fatal("included another reservation")
	}
}

func TestCloudUsageByReservationFailureKeepsHoldsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudUsageByReservation(path, map[uuid.UUID]bool{uuid.New(): true}); err == nil {
		t.Fatal("a malformed log was treated as no usage")
	}
}

type fakeCloudHoldWallet struct {
	reservations []wallet.Reservation
	settlements  []wallet.SettleRequest
	refunds      []wallet.RefundRequest
}

func (f *fakeCloudHoldWallet) StaleReservations(context.Context, time.Time, int) ([]wallet.Reservation, error) {
	return f.reservations, nil
}
func (f *fakeCloudHoldWallet) Settle(_ context.Context, req wallet.SettleRequest) (wallet.Settlement, error) {
	f.settlements = append(f.settlements, req)
	return wallet.Settlement{}, nil
}
func (f *fakeCloudHoldWallet) Refund(_ context.Context, req wallet.RefundRequest) (wallet.RefundResult, error) {
	f.refunds = append(f.refunds, req)
	return wallet.RefundResult{}, nil
}

func TestReconcileCloudHoldsActsOnlyOnKnownUsage(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		noRow       bool
		missingLog  bool
		wantErr     bool
		wantSettles int
		wantRefunds int
	}{
		{name: "no usage row leaves the hold", noRow: true},
		{name: "an absent log leaves the hold", missingLog: true, wantErr: true},
		{name: "a confirmed zero-usage failure refunds", status: 502, wantRefunds: 1},
		{name: "a successful zero-token call settles its fixed price", status: 200, wantSettles: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			path := filepath.Join(t.TempDir(), "usage.jsonl")
			if !tt.missingLog {
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if !tt.noRow {
					if err := json.NewEncoder(f).Encode(usage.Record{BillingReservationID: id.String(), Status: tt.status}); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			w := &fakeCloudHoldWallet{reservations: []wallet.Reservation{{ID: id}}}
			err := reconcileCloudHolds(context.Background(), w, path, time.Now())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(w.settlements) != tt.wantSettles || len(w.refunds) != tt.wantRefunds {
				t.Fatalf("%d settlements and %d refunds, want %d and %d", len(w.settlements), len(w.refunds), tt.wantSettles, tt.wantRefunds)
			}
		})
	}
}
