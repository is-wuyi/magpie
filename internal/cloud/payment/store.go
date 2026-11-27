package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Store is the payment package's persistence port. The reference
// implementation is PostgresStore; tests use an in-memory Store. A Store
// must make InTx atomic and give LockOrderByMerchantNo PostgreSQL's
// SELECT ... FOR UPDATE semantics: every callback settlement for one order
// then runs one at a time.
type Store interface {
	// InTx runs fn in one read-committed transaction and commits only when
	// fn returns nil. A rollback runs even when ctx was cancelled.
	InTx(ctx context.Context, fn func(context.Context, Tx) error) error

	// OrderByMerchantNo reads an order without locking it, for status
	// queries and administration.
	OrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error)

	// PendingCreditOrders lists success orders whose wallet credit is still
	// owed, oldest first, up to limit.
	PendingCreditOrders(ctx context.Context, limit int) ([]string, error)
}

// Tx is one transaction. The domain methods lock and write payment rows;
// Exec and QueryRow let an optional TransactionalCreditor write the wallet
// ledger in the same transaction.
type Tx interface {
	InsertOrder(ctx context.Context, o *Order) error
	LockOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error)
	SaveOrder(ctx context.Context, o *Order) error
	// InsertEvent returns false when (provider, provider_event_id) already
	// exists; that notification must not be settled again.
	InsertEvent(ctx context.Context, e *Event) (bool, error)
	// MarkCreditPendingEventsCredited moves every still-owed event of one
	// order to credited, so a retry path that has no event id of its own
	// still leaves an accurate audit trail.
	MarkCreditPendingEventsCredited(ctx context.Context, orderID uuid.UUID) error

	// Exec and QueryRow run SQL in this transaction. They are the same
	// method shapes as pgx.Tx's, so a wallet package can share its ledger
	// helpers with a TransactionalCreditor.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	// Raw returns the underlying pgx transaction for a TransactionalCreditor
	// whose ledger helpers take pgx.Tx. It is nil for a Store that is not
	// backed by pgx.
	Raw() pgx.Tx
}

// Pool is the subset of *pgxpool.Pool PostgresStore needs. It is an
// interface so an embedding application can supply a wrapped or instrumented
// pool.
type Pool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PostgresStore is the reference Store. It runs the statements in
// 003_payment.sql over the caller's pool; it opens no connection itself and
// owns nothing.
type PostgresStore struct {
	pool Pool
}

// NewPostgresStore wraps pool. A nil pool is refused before any query.
func NewPostgresStore(pool Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil database pool", ErrInvalidConfig)
	}
	return &PostgresStore{pool: pool}, nil
}

// InTx begins a read-committed transaction, gives it to fn and commits only
// when fn succeeds. The rollback uses a context that is not already
// cancelled, so a cancelled request still releases its locks and connection.
func (s *PostgresStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("payment: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(ctx, &pgxTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("payment: commit: %w", err)
	}
	return nil
}

// OrderByMerchantNo reads one order without locking it.
func (s *PostgresStore) OrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE merchant_order_no = $1`,
		merchantOrderNo)
	return scanOrder(row)
}

// PendingCreditOrders lists success orders whose wallet credit is still owed.
func (s *PostgresStore) PendingCreditOrders(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: pending credit limit %d", ErrInvalidConfig, limit)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT merchant_order_no FROM payment_orders
		 WHERE status = 'success' AND credit_state = 'pending'
		 ORDER BY updated_at, id
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: read pending credits: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return nil, fmt.Errorf("payment: read pending credits: %w", err)
		}
		out = append(out, no)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: read pending credits: %w", err)
	}
	return out, nil
}

// OrdersByUser lists one user's newest orders without exposing another
// tenant's merchant numbers or payment state.
func (s *PostgresStore) OrdersByUser(ctx context.Context, userID uuid.UUID, limit int) ([]Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+orderColumns+` FROM payment_orders WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	return out, rows.Err()
}

// orderColumns is the projection scanOrder reads. UUIDs are rendered as text
// and nullable times as pgtype values, so the scan does not depend on the
// driver's UUID or NULL handling.
const orderColumns = `id::text, merchant_order_no, user_id::text, provider,
	provider_trade_no, amount_fen, amount_micro_cny, currency, status,
	subject, credit_state, created_at, updated_at, expires_at, paid_at,
	credited_at`

// pgxTx adapts a pgx.Tx to the package's Tx interface.
type pgxTx struct {
	tx pgx.Tx
}

func (t *pgxTx) InsertOrder(ctx context.Context, o *Order) error {
	if err := o.validate(); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx,
		`INSERT INTO payment_orders
		 (id, merchant_order_no, user_id, provider, provider_trade_no,
		  amount_fen, amount_micro_cny, currency, status, subject,
		  credit_state, created_at, updated_at, expires_at, paid_at,
		  credited_at)
		 VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10,
		         $11, $12, $13, $14, $15, $16)`,
		o.ID.String(), o.MerchantOrderNo, o.UserID.String(), string(o.Provider),
		nullableString(o.ProviderTradeNo), int64(o.AmountFen), int64(o.AmountMicroCNY),
		o.Currency, string(o.Status), o.Subject, string(o.CreditState),
		o.CreatedAt, o.UpdatedAt, o.ExpiresAt, nullableTime(o.PaidAt), nullableTime(o.CreditedAt))
	if err != nil {
		return fmt.Errorf("payment: insert order %s: %w", o.MerchantOrderNo, err)
	}
	return nil
}

func (t *pgxTx) LockOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error) {
	row := t.tx.QueryRow(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE merchant_order_no = $1 FOR UPDATE`,
		merchantOrderNo)
	return scanOrder(row)
}

func (t *pgxTx) SaveOrder(ctx context.Context, o *Order) error {
	tag, err := t.tx.Exec(ctx,
		`UPDATE payment_orders
		 SET provider_trade_no = $2, status = $3, credit_state = $4,
		     paid_at = $5, credited_at = $6, updated_at = $7
		 WHERE id = $1::uuid`,
		o.ID.String(), nullableString(o.ProviderTradeNo), string(o.Status),
		string(o.CreditState), nullableTime(o.PaidAt), nullableTime(o.CreditedAt),
		o.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "payment_orders_provider_trade_no_idx" {
			return fmt.Errorf("%w: provider trade %s already pays another order", ErrTradeNoMismatch, o.ProviderTradeNo)
		}
		return fmt.Errorf("payment: save order %s: %w", o.MerchantOrderNo, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: order %s disappeared mid-transaction", ErrOrderNotFound, o.MerchantOrderNo)
	}
	return nil
}

func (t *pgxTx) InsertEvent(ctx context.Context, e *Event) (bool, error) {
	tag, err := t.tx.Exec(ctx,
		`INSERT INTO payment_events
		 (id, provider, provider_event_id, order_id, merchant_order_no,
		  event_type, provider_trade_no, amount_fen, signature_verified,
		  payload_sha256, outcome, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10,
		         $11, $12, $13)
		 ON CONFLICT (provider, provider_event_id) DO NOTHING`,
		e.ID.String(), string(e.Provider), e.ProviderEventID, e.OrderID.String(),
		e.MerchantOrderNo, e.EventType, nullableString(e.ProviderTradeNo),
		int64(e.AmountFen), e.SignatureVerified, e.PayloadSHA256,
		string(e.Outcome), e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("payment: insert event %s: %w", e.ProviderEventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (t *pgxTx) MarkCreditPendingEventsCredited(ctx context.Context, orderID uuid.UUID) error {
	if _, err := t.tx.Exec(ctx,
		`UPDATE payment_events SET outcome = $2, updated_at = now()
		 WHERE order_id = $1::uuid AND outcome = $3`,
		orderID.String(), string(EventCredited), string(EventCreditPending)); err != nil {
		return fmt.Errorf("payment: mark pending credit events for %s: %w", orderID, err)
	}
	return nil
}

func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

func (t *pgxTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t *pgxTx) Raw() pgx.Tx { return t.tx }

// scanOrder reads the orderColumns projection.
func scanOrder(row pgx.Row) (*Order, error) {
	var (
		o                      Order
		id, userID             string
		provider, status, cred string
		tradeNo                pgtype.Text
		amountFen, amountMicro int64
		paidAt, creditedAt     pgtype.Timestamptz
	)
	err := row.Scan(&id, &o.MerchantOrderNo, &userID, &provider, &tradeNo,
		&amountFen, &amountMicro, &o.Currency, &status, &o.Subject, &cred,
		&o.CreatedAt, &o.UpdatedAt, &o.ExpiresAt, &paidAt, &creditedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("payment: read order: %w", err)
	}
	if o.ID, err = uuid.Parse(id); err != nil {
		return nil, fmt.Errorf("%w: order id %q", ErrInconsistentState, id)
	}
	if o.UserID, err = uuid.Parse(userID); err != nil {
		return nil, fmt.Errorf("%w: user id %q", ErrInconsistentState, userID)
	}
	o.Provider = Provider(provider)
	o.Status = Status(status)
	o.CreditState = CreditState(cred)
	o.AmountFen = Fen(amountFen)
	o.AmountMicroCNY = MicroCNY(amountMicro)
	if tradeNo.Valid {
		o.ProviderTradeNo = tradeNo.String
	}
	if paidAt.Valid {
		t := paidAt.Time
		o.PaidAt = &t
	}
	if creditedAt.Valid {
		t := creditedAt.Time
		o.CreditedAt = &t
	}
	return &o, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
