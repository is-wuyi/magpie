// Package wallet keeps each user's prepaid balance in PostgreSQL and charges
// it for gateway calls through a hold/settle/refund cycle.
//
// All money is int64 micro-CNY (one CNY is 1,000,000 micros). Every amount,
// hold and ledger row is persisted: a new Service on the same database sees
// all earlier state, including holds made by another process.
//
// The cycle:
//   - Credit adds money to a wallet. It is idempotent by the caller's payment
//     key, so a payment callback can replay it without adding money twice.
//   - Reserve locks the user's wallet, refuses when balance-held is below the
//     estimated cost, and writes a reservation plus an immutable hold entry.
//     The price is snapshotted into the reservation, so a later price change
//     cannot reprice an in-flight call. Reserving the same (user, request id)
//     again returns the first reservation and takes no second hold.
//   - Settle locks the reservation, charges actual usage at the snapshot
//     price and releases the whole hold. Settling a settled reservation
//     returns the first settlement without debiting again, from any Service
//     or process.
//   - Refund releases a held reservation with no charge, for a call that will
//     not run. It is idempotent the same way.
//   - RecoverStaleHolds refunds held reservations whose expiry passed.
//     Callers run it periodically because a crashed request cannot release its
//     own hold. Reads never expire a hold by themselves.
//
// A settlement may charge more than its hold when actual usage exceeds the
// estimate. The balance may then go below zero (a debt), and new holds are
// refused until the user is credited. A settled charge is not refunded by
// Refund; use Credit for an adjustment.
//
// The schema is internal/cloud/store/migrations/002_billing.sql and expects
// users(id uuid) from 001_identity.sql. Statements use unqualified table
// names, so the pool's search_path selects the schema.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

const (
	// DefaultHoldTTL is how long a reservation made without an explicit TTL
	// holds its money before RecoverStaleHolds may release it. A request that
	// can outlive it must pass a longer TTL: once a hold is recovered, its
	// reservation is refunded and Settle refuses it.
	DefaultHoldTTL = 30 * time.Minute

	// MaxRequestIDLength and MaxIdempotencyKeyLength match the text bounds
	// in 002_billing.sql.
	MaxRequestIDLength      = 200
	MaxIdempotencyKeyLength = 200

	// MaxDescriptionLength matches the ledger's description bound.
	MaxDescriptionLength = 500

	// maxRecoveryBatch bounds one RecoverStaleHolds call.
	maxRecoveryBatch = 1000

	// internalIDKeyPrefix marks ledger keys magpie writes for reservation
	// operations. Credit refuses caller keys with this prefix so a payment
	// key can never collide with one.
	internalIDKeyPrefix = "magpie-reservation:"

	// openPingTimeout bounds Open's reachability check. A caller with a
	// shorter deadline keeps it.
	openPingTimeout = 10 * time.Second
)

// Sentinel errors. A database failure is returned as its own error and is
// never translated into one of these.
var (
	// ErrInvalidRequest means a request field is missing or out of range.
	ErrInvalidRequest = errors.New("wallet: invalid request")

	// ErrInvalidDatabaseURL means Open could not parse its URL. The raw URL
	// is left out because it may contain a password.
	ErrInvalidDatabaseURL = errors.New("wallet: the database URL is not valid")

	// ErrInsufficientFunds means balance-held is below the requested hold.
	// The wallet is unchanged.
	ErrInsufficientFunds = errors.New("wallet: insufficient available balance")

	// ErrReservationNotFound means no reservation has that id.
	ErrReservationNotFound = errors.New("wallet: reservation not found")

	// ErrReservationSettled means the reservation was already charged.
	ErrReservationSettled = errors.New("wallet: reservation already settled")

	// ErrReservationRefunded means the reservation was already released.
	ErrReservationRefunded = errors.New("wallet: reservation already refunded")

	// ErrIdempotencyConflict means an idempotency key is already recorded
	// for another user or another kind of operation.
	ErrIdempotencyConflict = errors.New("wallet: idempotency key belongs to another operation")

	// ErrAmountOverflow means a balance arithmetic result would not fit in
	// an int64. The transaction is rolled back.
	ErrAmountOverflow = errors.New("wallet: amount overflows int64")

	// ErrWalletState means stored rows disagree with each other: for
	// example a reservation holds more than its wallet's held total. It is
	// never retried automatically.
	ErrWalletState = errors.New("wallet: wallet state is inconsistent")
)

// InsufficientFundsError reports the two amounts that made Reserve refuse. It
// unwraps to ErrInsufficientFunds.
type InsufficientFundsError struct {
	Need      pricing.Micros
	Available pricing.Micros
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("%s: need %s, %s available", ErrInsufficientFunds, e.Need, e.Available)
}

func (e *InsufficientFundsError) Unwrap() error { return ErrInsufficientFunds }

// Service is the PostgreSQL-backed wallet. It is safe for concurrent use.
type Service struct {
	pool           *pgxpool.Pool
	ownsPool       bool
	now            func() time.Time
	defaultHoldTTL time.Duration
}

// Option changes a Service before it is used.
type Option func(*Service)

// WithClock replaces the wall clock, for tests and deterministic callers. A
// nil clock is ignored.
func WithClock(clock func() time.Time) Option {
	return func(s *Service) {
		if clock != nil {
			s.now = clock
		}
	}
}

// WithDefaultHoldTTL changes the lifetime of a reservation that does not name
// its own TTL. New rejects a non-positive value instead of silently using the
// default.
func WithDefaultHoldTTL(ttl time.Duration) Option {
	return func(s *Service) { s.defaultHoldTTL = ttl }
}

// New wraps an existing pool. The caller keeps ownership of the pool and must
// close it; Service.Close does nothing for such a Service.
//
// New does not contact the database. A nil pool or an unusable option is
// refused here, before any query.
func New(pool *pgxpool.Pool, opts ...Option) (*Service, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pgxpool", ErrInvalidRequest)
	}
	s := &Service{
		pool:           pool,
		now:            time.Now,
		defaultHoldTTL: DefaultHoldTTL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.now == nil {
		return nil, fmt.Errorf("%w: clock is nil", ErrInvalidRequest)
	}
	if s.defaultHoldTTL <= 0 {
		return nil, fmt.Errorf("%w: default hold TTL %s is not positive", ErrInvalidRequest, s.defaultHoldTTL)
	}
	return s, nil
}

// Open parses databaseURL, creates a pool, and checks that PostgreSQL
// answers. The returned Service owns the pool; Close releases it.
//
// Open does not apply migrations. Run the store's migrations (at least
// 002_billing.sql, after 001_identity.sql) first; an unmigrated database makes
// every wallet method fail rather than serve a wrong balance.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*Service, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("%w: database URL is empty", ErrInvalidRequest)
	}
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx includes the raw URL, and sometimes its password, in parse
		// errors. Keep that out of logs and replies.
		return nil, ErrInvalidDatabaseURL
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("wallet: create connection pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, openPingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("wallet: database is not reachable: %w", err)
	}
	s, err := New(pool, opts...)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownsPool = true
	return s, nil
}

// Close releases a pool created by Open. For a Service made with New it is a
// no-op: the caller still owns the pool.
func (s *Service) Close() {
	if s != nil && s.ownsPool && s.pool != nil {
		s.pool.Close()
		s.ownsPool = false
	}
}

// Balance returns the stored balance, holds and their difference. A user with
// no wallet row has a zero balance, not an error: the row appears with the
// first credit or reservation.
func (s *Service) Balance(ctx context.Context, userID uuid.UUID) (Balance, error) {
	if err := s.usable(); err != nil {
		return Balance{}, err
	}
	if err := validateUserID(userID); err != nil {
		return Balance{}, err
	}
	var balance, held int64
	err := s.pool.QueryRow(ctx,
		`SELECT balance_micros, held_micros FROM wallets WHERE user_id = $1::uuid`,
		userID.String()).Scan(&balance, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{UserID: userID}, nil
	}
	if err != nil {
		return Balance{}, fmt.Errorf("wallet: read balance: %w", err)
	}
	available, err := subInt64(balance, held)
	if err != nil {
		return Balance{}, fmt.Errorf("%w: wallet %s balance %d held %d", ErrWalletState, userID, balance, held)
	}
	return Balance{
		UserID:          userID,
		BalanceMicros:   pricing.Micros(balance),
		HeldMicros:      pricing.Micros(held),
		AvailableMicros: pricing.Micros(available),
	}, nil
}

// currentTime returns the Service clock in UTC and refuses a zero reading.
func (s *Service) currentTime() (time.Time, error) {
	if s == nil || s.now == nil {
		return time.Time{}, fmt.Errorf("%w: wallet service is not initialized; use New or Open", ErrInvalidRequest)
	}
	now := s.now().UTC()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("%w: clock returned the zero time", ErrInvalidRequest)
	}
	return now, nil
}

// usable refuses a zero-value Service. New and Open are the only supported
// constructors; an uninitialized value must not panic on the first query.
func (s *Service) usable() error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("%w: wallet service is not initialized; use New or Open", ErrInvalidRequest)
	}
	return nil
}
