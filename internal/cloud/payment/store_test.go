package payment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// captureTx embeds pgx.Tx for the methods this test does not call and records
// the lock query. It proves the callback's lock is SELECT ... FOR UPDATE, the
// property the in-memory store models.
type captureTx struct {
	pgx.Tx
	query string
}

func (t *captureTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.query = sql
	return errRow{err: pgx.ErrNoRows}
}

func TestPostgresLockOrderUsesForUpdate(t *testing.T) {
	tx := &captureTx{}
	_, err := (&pgxTx{tx: tx}).LockOrderByMerchantNo(context.Background(), "MTEST000001")
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("LockOrderByMerchantNo on a missing row = %v, want ErrOrderNotFound", err)
	}
	if !strings.Contains(tx.query, "FOR UPDATE") {
		t.Fatalf("order lock query = %q, want SELECT ... FOR UPDATE", tx.query)
	}
}

// A unique provider trade number raised by the database must be the same
// error the callback path returns for a duplicate payment.
type execTx struct {
	pgx.Tx
	query string
	tag   pgconn.CommandTag
	err   error
}

func (t *execTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.query = sql
	return t.tag, t.err
}

func TestPostgresSaveOrderNamesTradeNoConflict(t *testing.T) {
	o := &Order{
		ID:              uuid.MustParse("00000000-0000-4000-8000-000000000001"),
		MerchantOrderNo: "MTEST000001",
		Provider:        ProviderAlipay,
		ProviderTradeNo: "trade-1",
		Status:          StatusSuccess,
		CreditState:     CreditPending,
	}
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "payment_orders_provider_trade_no_idx"}
	err := (&pgxTx{tx: &execTx{err: pgErr}}).SaveOrder(context.Background(), o)
	if !errors.Is(err, ErrTradeNoMismatch) {
		t.Fatalf("unique trade number error = %v, want ErrTradeNoMismatch", err)
	}

	tx := &execTx{tag: pgconn.NewCommandTag("UPDATE 1")}
	if err := (&pgxTx{tx: tx}).SaveOrder(context.Background(), o); err != nil {
		t.Fatalf("SaveOrder: %v", err)
	}
}

func TestPostgresInsertEventReportsConflicts(t *testing.T) {
	e := &Event{
		ID:              uuid.MustParse("00000000-0000-4000-8000-000000000002"),
		Provider:        ProviderAlipay,
		ProviderEventID: "ALI-1",
	}
	for _, tc := range []struct {
		tag  pgconn.CommandTag
		want bool
	}{
		{pgconn.NewCommandTag("INSERT 0 1"), true},
		{pgconn.NewCommandTag("INSERT 0 0"), false},
	} {
		tx := &execTx{tag: tc.tag}
		created, err := (&pgxTx{tx: tx}).InsertEvent(context.Background(), e)
		if err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
		if created != tc.want {
			t.Errorf("InsertEvent with tag %q = %v, want %v", tc.tag, created, tc.want)
		}
		if !strings.Contains(tx.query, "ON CONFLICT (provider, provider_event_id) DO NOTHING") {
			t.Errorf("InsertEvent query = %q, want ON CONFLICT DO NOTHING", tx.query)
		}
	}
}
