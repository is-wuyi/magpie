package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

// reservationColumns is the projection scanReservation reads. charged_micros
// is nullable while a reservation is held; COALESCE keeps the scan simple.
const reservationColumns = `id::text, user_id::text, request_id, status,
	price_snapshot, estimated_usage, hold_micros, COALESCE(charged_micros, 0),
	expires_at, created_at, updated_at`

// ledgerEntryColumns is the projection scanLedgerEntry reads.
const ledgerEntryColumns = `id::text, user_id::text, COALESCE(reservation_id::text, ''),
	kind, balance_delta_micros, held_delta_micros, balance_after_micros,
	held_after_micros, idempotency_key, created_at`

// inTx runs fn in a read-committed transaction and commits only when fn
// returns nil. A rollback uses a context that is not already cancelled, so a
// cancelled request still releases its locks and connection.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if err := s.usable(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("wallet: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("wallet: commit: %w", err)
	}
	return nil
}

// ensureAndLockWallet creates the user's wallet if this is the first write,
// then locks it until the transaction ends. Every write for one user takes
// this lock, which serializes holds, settlements and credits for that user.
func ensureAndLockWallet(ctx context.Context, tx pgx.Tx, userID uuid.UUID, now time.Time) (balance, held int64, err error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO wallets (user_id, created_at, updated_at) VALUES ($1::uuid, $2, $2)
		 ON CONFLICT (user_id) DO NOTHING`,
		userID.String(), now); err != nil {
		return 0, 0, fmt.Errorf("wallet: create wallet: %w", err)
	}
	return lockWallet(ctx, tx, userID)
}

// lockWallet locks an existing wallet. A missing row is an inconsistent
// state: every reservation and credit creates its wallet first.
func lockWallet(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (balance, held int64, err error) {
	err = tx.QueryRow(ctx,
		`SELECT balance_micros, held_micros FROM wallets WHERE user_id = $1::uuid FOR UPDATE`,
		userID.String()).Scan(&balance, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("%w: no wallet row for user %s", ErrWalletState, userID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("wallet: lock wallet: %w", err)
	}
	return balance, held, nil
}

// updateWallet writes both totals and bumps the version. The row is already
// locked by the caller.
func updateWallet(ctx context.Context, tx pgx.Tx, userID uuid.UUID, balance, held int64, now time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE wallets
		 SET balance_micros = $2, held_micros = $3, version = version + 1, updated_at = $4
		 WHERE user_id = $1::uuid`,
		userID.String(), balance, held, now)
	if err != nil {
		return fmt.Errorf("wallet: update wallet: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s disappeared mid-transaction", ErrWalletState, userID)
	}
	return nil
}

// ledgerEntry is one wallet_entries row.
type ledgerEntry struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ReservationID  uuid.UUID
	Kind           string
	BalanceDelta   int64
	HeldDelta      int64
	BalanceAfter   int64
	HeldAfter      int64
	IdempotencyKey string
	Description    string
	CreatedAt      time.Time
}

// insertLedgerEntry appends one immutable entry. A reservation entry may name
// a reservation this transaction inserts later: the foreign key is deferred.
func insertLedgerEntry(ctx context.Context, tx pgx.Tx, e ledgerEntry) error {
	var reservation any
	if e.ReservationID != uuid.Nil {
		reservation = e.ReservationID.String()
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO wallet_entries
		 (id, user_id, reservation_id, kind, balance_delta_micros,
		  held_delta_micros, balance_after_micros, held_after_micros,
		  idempotency_key, description, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.ID.String(), e.UserID.String(), reservation, e.Kind,
		e.BalanceDelta, e.HeldDelta, e.BalanceAfter, e.HeldAfter,
		e.IdempotencyKey, e.Description, e.CreatedAt)
	if err != nil {
		return mapLedgerInsertError(err, e)
	}
	return nil
}

// mapLedgerInsertError names the two unique constraints Reserve, Settle and
// Refund rely on for idempotency. Anything else is a database failure.
func mapLedgerInsertError(err error, e ledgerEntry) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "wallet_entries_idempotency_key_key":
			return fmt.Errorf("%w: ledger key %q for %s", ErrIdempotencyConflict, e.IdempotencyKey, e.UserID)
		case "wallet_entries_reservation_kind_idx":
			return fmt.Errorf("%w: reservation %s already has a %s entry", ErrWalletState, e.ReservationID, e.Kind)
		}
	}
	return fmt.Errorf("wallet: insert ledger entry: %w", err)
}

// entryByIDKey reads the credit entry a payment key already produced.
func entryByIDKey(ctx context.Context, tx pgx.Tx, key string) (ledgerEntry, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+ledgerEntryColumns+` FROM wallet_entries WHERE idempotency_key = $1`, key)
	return scanLedgerEntry(row)
}

// entryByReservationKind reads the settle or refund entry of a reservation.
func entryByReservationKind(ctx context.Context, tx pgx.Tx, reservationID uuid.UUID, kind string) (ledgerEntry, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+ledgerEntryColumns+`
		 FROM wallet_entries WHERE reservation_id = $1::uuid AND kind = $2`,
		reservationID.String(), kind)
	return scanLedgerEntry(row)
}

// reservationByRequest finds an existing reservation for a retried Reserve.
func reservationByRequest(ctx context.Context, tx pgx.Tx, userID uuid.UUID, requestID string) (Reservation, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+reservationColumns+`
		 FROM billing_reservations WHERE user_id = $1::uuid AND request_id = $2`,
		userID.String(), requestID)
	return scanReservation(row)
}

// lockReservation locks one reservation until the transaction ends. It is
// taken before the wallet lock, so two settlements of one reservation cannot
// both see status = held.
func lockReservation(ctx context.Context, tx pgx.Tx, reservationID uuid.UUID) (Reservation, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+reservationColumns+`
		 FROM billing_reservations WHERE id = $1::uuid FOR UPDATE`,
		reservationID.String())
	res, err := scanReservation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, fmt.Errorf("%w: %s", ErrReservationNotFound, reservationID)
	}
	return res, err
}

// insertReservation writes the hold row. holdEntryID names the entry written
// just before it in the same transaction.
func insertReservation(ctx context.Context, tx pgx.Tx, r Reservation, holdEntryID uuid.UUID, snapshotJSON, estimatedJSON string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO billing_reservations
		 (id, user_id, request_id, status, channel, provider, model,
		  price_snapshot, estimated_usage, hold_micros, hold_entry_id,
		  expires_at, created_at, updated_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb,
		         $10, $11::uuid, $12, $13, $13)`,
		r.ID.String(), r.UserID.String(), r.RequestID, string(r.Status),
		r.Snapshot.Key.Channel, r.Snapshot.Key.Provider, r.Snapshot.Key.Model,
		snapshotJSON, estimatedJSON, int64(r.HoldMicros), holdEntryID.String(),
		r.ExpiresAt, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("wallet: insert reservation: %w", err)
	}
	return nil
}

// insertUsageRecord writes the settled call's usage. reservation_id is unique
// in the table, so a replayed settlement cannot add a second row.
func insertUsageRecord(ctx context.Context, tx pgx.Tx, id uuid.UUID, r Reservation, usage pricing.Usage, charge pricing.Micros, snapshotJSON string, now time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO usage_records
		 (id, user_id, reservation_id, request_id, channel, provider, model,
		  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		  cost_micros, price_snapshot, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7,
		         $8, $9, $10, $11, $12, $13::jsonb, $14)`,
		id.String(), r.UserID.String(), r.ID.String(), r.RequestID,
		r.Snapshot.Key.Channel, r.Snapshot.Key.Provider, r.Snapshot.Key.Model,
		usage.InputTokens, usage.OutputTokens,
		usage.CacheReadTokens, usage.CacheWriteTokens,
		int64(charge), snapshotJSON, now)
	if err != nil {
		return fmt.Errorf("wallet: insert usage record: %w", err)
	}
	return nil
}

func scanReservation(row pgx.Row) (Reservation, error) {
	var idText, userText, requestID, status string
	var snapshotJSON, estimatedJSON []byte
	var hold, charged int64
	var expiresAt, createdAt, updatedAt time.Time
	if err := row.Scan(&idText, &userText, &requestID, &status,
		&snapshotJSON, &estimatedJSON, &hold, &charged,
		&expiresAt, &createdAt, &updatedAt); err != nil {
		return Reservation{}, err
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation id %q", ErrWalletState, idText)
	}
	userID, err := uuid.Parse(userText)
	if err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation user %q", ErrWalletState, userText)
	}
	st := Status(status)
	switch st {
	case StatusHeld, StatusSettled, StatusRefunded:
	default:
		return Reservation{}, fmt.Errorf("%w: reservation %s has status %q", ErrWalletState, id, status)
	}
	var snapshot pricing.Snapshot
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation %s price snapshot: %w", ErrWalletState, id, err)
	}
	if err := snapshot.Validate(); err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation %s price snapshot: %w", ErrWalletState, id, err)
	}
	var usage pricing.Usage
	if err := json.Unmarshal(estimatedJSON, &usage); err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation %s estimated usage: %w", ErrWalletState, id, err)
	}
	if err := usage.Validate(); err != nil {
		return Reservation{}, fmt.Errorf("%w: reservation %s estimated usage: %w", ErrWalletState, id, err)
	}
	return Reservation{
		ID:             id,
		UserID:         userID,
		RequestID:      requestID,
		Status:         st,
		Snapshot:       snapshot,
		EstimatedUsage: usage,
		HoldMicros:     pricing.Micros(hold),
		ChargedMicros:  pricing.Micros(charged),
		ExpiresAt:      expiresAt.UTC(),
		CreatedAt:      createdAt.UTC(),
		UpdatedAt:      updatedAt.UTC(),
	}, nil
}

func scanLedgerEntry(row pgx.Row) (ledgerEntry, error) {
	var idText, userText, reservationText, kind, key string
	var balanceDelta, heldDelta, balanceAfter, heldAfter int64
	var createdAt time.Time
	if err := row.Scan(&idText, &userText, &reservationText, &kind,
		&balanceDelta, &heldDelta, &balanceAfter, &heldAfter, &key, &createdAt); err != nil {
		return ledgerEntry{}, err
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return ledgerEntry{}, fmt.Errorf("%w: ledger id %q", ErrWalletState, idText)
	}
	userID, err := uuid.Parse(userText)
	if err != nil {
		return ledgerEntry{}, fmt.Errorf("%w: ledger user %q", ErrWalletState, userText)
	}
	var reservationID uuid.UUID
	if reservationText != "" {
		reservationID, err = uuid.Parse(reservationText)
		if err != nil {
			return ledgerEntry{}, fmt.Errorf("%w: ledger reservation %q", ErrWalletState, reservationText)
		}
	}
	return ledgerEntry{
		ID:             id,
		UserID:         userID,
		ReservationID:  reservationID,
		Kind:           kind,
		BalanceDelta:   balanceDelta,
		HeldDelta:      heldDelta,
		BalanceAfter:   balanceAfter,
		HeldAfter:      heldAfter,
		IdempotencyKey: key,
		CreatedAt:      createdAt.UTC(),
	}, nil
}

// validateUserID refuses the zero UUID, which is never a user.
func validateUserID(userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("%w: user id is empty", ErrInvalidRequest)
	}
	return nil
}

// validateRequiredText checks a non-empty text field against the same bound
// as its column. Length is counted in characters, as PostgreSQL's length()
// does.
func validateRequiredText(name, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidRequest, name)
	}
	return validateText(name, value, max)
}

// validateOptionalText checks a field that may be empty.
func validateOptionalText(name, value string, max int) error {
	if value == "" {
		return nil
	}
	return validateText(name, value, max)
}

func validateText(name, value string, max int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidRequest, name)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: %s contains a NUL byte", ErrInvalidRequest, name)
	}
	if utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%w: %s is longer than %d characters", ErrInvalidRequest, name, max)
	}
	return nil
}

// validateCreditKey reserves magpie's internal ledger prefix for reservation
// entries.
func validateCreditKey(key string) error {
	if err := validateRequiredText("idempotency key", key, MaxIdempotencyKeyLength); err != nil {
		return err
	}
	if strings.HasPrefix(key, internalIDKeyPrefix) {
		return fmt.Errorf("%w: idempotency key uses the reserved prefix %q", ErrInvalidRequest, internalIDKeyPrefix)
	}
	return nil
}

func addInt64(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, ErrAmountOverflow
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}

func subInt64(a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, ErrAmountOverflow
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, ErrAmountOverflow
	}
	return a - b, nil
}
