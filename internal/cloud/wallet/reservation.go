package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

// Reserve takes a hold for one request, or returns the reservation an earlier
// call with the same (user, request id) made, even if that call quoted a
// different price.
//
// The transaction locks the user's wallet, checks the available balance, and
// writes the hold entry and the reservation together. The price quote is
// stored in the reservation, so the settlement uses these rates even if the
// price table changes. A hold that does not fit is refused with
// *InsufficientFundsError and changes nothing.
func (s *Service) Reserve(ctx context.Context, req ReserveRequest) (Reservation, error) {
	if err := s.usable(); err != nil {
		return Reservation{}, err
	}
	if err := s.validateReserve(req); err != nil {
		return Reservation{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return Reservation{}, err
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = s.defaultHoldTTL
	}
	expiresAt := now.Add(ttl)
	if !expiresAt.After(now) {
		return Reservation{}, fmt.Errorf("%w: hold TTL %s does not reach the future", ErrInvalidRequest, ttl)
	}
	hold, err := req.Quote.Cost(req.EstimatedUsage)
	if err != nil {
		return Reservation{}, fmt.Errorf("wallet: reserve price: %w", err)
	}
	snapshot, err := pricing.NewSnapshot(req.Quote, now)
	if err != nil {
		return Reservation{}, fmt.Errorf("wallet: reserve price snapshot: %w", err)
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return Reservation{}, fmt.Errorf("wallet: encode price snapshot: %w", err)
	}
	estimatedJSON, err := json.Marshal(req.EstimatedUsage)
	if err != nil {
		return Reservation{}, fmt.Errorf("wallet: encode estimated usage: %w", err)
	}

	var out Reservation
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		balance, held, err := ensureAndLockWallet(ctx, tx, req.UserID, now)
		if err != nil {
			return err
		}
		// The wallet lock serializes every reservation for this user, so
		// this read cannot miss a reservation another process is inserting
		// for the same request id.
		existing, err := reservationByRequest(ctx, tx, req.UserID, req.RequestID)
		if err == nil {
			existing.Replayed = true
			out = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("wallet: read reservation for request %q: %w", req.RequestID, err)
		}

		available, err := subInt64(balance, held)
		if err != nil {
			return fmt.Errorf("%w: user %s balance %d held %d", ErrWalletState, req.UserID, balance, held)
		}
		if int64(hold) > available {
			return &InsufficientFundsError{Need: hold, Available: pricing.Micros(available)}
		}
		newHeld, err := addInt64(held, int64(hold))
		if err != nil {
			return fmt.Errorf("wallet: reserve hold: %w", err)
		}
		reservationID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate reservation id: %w", err)
		}
		entryID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate hold entry id: %w", err)
		}
		if err := insertLedgerEntry(ctx, tx, ledgerEntry{
			ID:             entryID,
			UserID:         req.UserID,
			ReservationID:  reservationID,
			Kind:           "hold",
			BalanceDelta:   0,
			HeldDelta:      int64(hold),
			BalanceAfter:   balance,
			HeldAfter:      newHeld,
			IdempotencyKey: internalIDKeyPrefix + reservationID.String() + ":hold",
			Description:    req.Description,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		if err := updateWallet(ctx, tx, req.UserID, balance, newHeld, now); err != nil {
			return err
		}
		res := Reservation{
			ID:             reservationID,
			UserID:         req.UserID,
			RequestID:      req.RequestID,
			Status:         StatusHeld,
			Snapshot:       snapshot,
			EstimatedUsage: req.EstimatedUsage,
			HoldMicros:     hold,
			ExpiresAt:      expiresAt,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := insertReservation(ctx, tx, res, entryID, string(snapshotJSON), string(estimatedJSON)); err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return out, nil
}

// Settle charges actual usage at the reservation's snapshot price and releases
// its hold.
//
// The reservation row is locked first, so two processes settling the same
// reservation cannot both debit: the first commits status = settled, the
// second sees it and returns the recorded settlement (Replayed = true)
// without touching the wallet. A replay reports the first settlement even
// when its usage differs; usage is never re-priced. Actual usage may exceed
// the hold; the wallet balance then goes below zero by the difference.
func (s *Service) Settle(ctx context.Context, req SettleRequest) (Settlement, error) {
	if err := s.usable(); err != nil {
		return Settlement{}, err
	}
	if req.ReservationID == uuid.Nil {
		return Settlement{}, fmt.Errorf("%w: reservation id is empty", ErrInvalidRequest)
	}
	if err := req.ActualUsage.Validate(); err != nil {
		return Settlement{}, fmt.Errorf("wallet: settle usage: %w", err)
	}
	if err := validateOptionalText("description", req.Description, MaxDescriptionLength); err != nil {
		return Settlement{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return Settlement{}, err
	}
	description := req.Description
	if description == "" {
		description = "settle"
	}

	var out Settlement
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		res, err := lockReservation(ctx, tx, req.ReservationID)
		if err != nil {
			return err
		}
		switch res.Status {
		case StatusSettled:
			entry, err := entryByReservationKind(ctx, tx, res.ID, "settle")
			if err != nil {
				return ledgerEntryReadError(err, res.ID, "settle")
			}
			if entry.BalanceDelta != -int64(res.ChargedMicros) || entry.HeldDelta != -int64(res.HoldMicros) {
				return fmt.Errorf("%w: settled reservation %s disagrees with its ledger", ErrWalletState, res.ID)
			}
			out = Settlement{
				ReservationID:  res.ID,
				EntryID:        entry.ID,
				UserID:         res.UserID,
				ChargedMicros:  res.ChargedMicros,
				ReleasedMicros: res.HoldMicros,
				UnusedMicros:   unusedHold(res.HoldMicros, res.ChargedMicros),
				BalanceMicros:  pricing.Micros(entry.BalanceAfter),
				Replayed:       true,
			}
			return nil
		case StatusRefunded:
			return fmt.Errorf("%w: %s", ErrReservationRefunded, res.ID)
		case StatusHeld:
		default:
			return fmt.Errorf("%w: reservation %s has status %q", ErrWalletState, res.ID, res.Status)
		}

		charge, err := res.Snapshot.Cost(req.ActualUsage)
		if err != nil {
			return fmt.Errorf("wallet: settle price: %w", err)
		}
		snapshotJSON, err := json.Marshal(res.Snapshot)
		if err != nil {
			return fmt.Errorf("wallet: encode reservation price snapshot: %w", err)
		}
		balance, held, err := lockWallet(ctx, tx, res.UserID)
		if err != nil {
			return err
		}
		if int64(res.HoldMicros) > held {
			return fmt.Errorf("%w: reservation %s holds %d but wallet %s holds %d",
				ErrWalletState, res.ID, res.HoldMicros, res.UserID, held)
		}
		newBalance, err := subInt64(balance, int64(charge))
		if err != nil {
			return fmt.Errorf("wallet: settle charge: %w", err)
		}
		newHeld, err := subInt64(held, int64(res.HoldMicros))
		if err != nil {
			return fmt.Errorf("%w: reservation %s release: %v", ErrWalletState, res.ID, err)
		}
		entryID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate settlement entry id: %w", err)
		}
		usageID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate usage id: %w", err)
		}
		if err := insertLedgerEntry(ctx, tx, ledgerEntry{
			ID:             entryID,
			UserID:         res.UserID,
			ReservationID:  res.ID,
			Kind:           "settle",
			BalanceDelta:   -int64(charge),
			HeldDelta:      -int64(res.HoldMicros),
			BalanceAfter:   newBalance,
			HeldAfter:      newHeld,
			IdempotencyKey: internalIDKeyPrefix + res.ID.String() + ":settle",
			Description:    description,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		if err := updateWallet(ctx, tx, res.UserID, newBalance, newHeld, now); err != nil {
			return err
		}
		if err := insertUsageRecord(ctx, tx, usageID, res, req.ActualUsage, charge, string(snapshotJSON), now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE billing_reservations
			 SET status = 'settled', charged_micros = $2, settlement_entry_id = $3::uuid,
			     settled_at = $4, updated_at = $4
			 WHERE id = $1::uuid AND status = 'held'`,
			res.ID.String(), int64(charge), entryID.String(), now)
		if err != nil {
			return fmt.Errorf("wallet: mark reservation settled: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: reservation %s changed while settling", ErrWalletState, res.ID)
		}
		out = Settlement{
			ReservationID:  res.ID,
			EntryID:        entryID,
			UserID:         res.UserID,
			ChargedMicros:  charge,
			ReleasedMicros: res.HoldMicros,
			UnusedMicros:   unusedHold(res.HoldMicros, charge),
			BalanceMicros:  pricing.Micros(newBalance),
		}
		return nil
	})
	if err != nil {
		return Settlement{}, err
	}
	return out, nil
}

// Refund releases a held reservation with no charge. A refunded reservation
// returns its recorded refund (Replayed = true); a settled one is refused with
// ErrReservationSettled.
func (s *Service) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	if err := s.usable(); err != nil {
		return RefundResult{}, err
	}
	if req.ReservationID == uuid.Nil {
		return RefundResult{}, fmt.Errorf("%w: reservation id is empty", ErrInvalidRequest)
	}
	if err := validateOptionalText("reason", req.Reason, MaxDescriptionLength); err != nil {
		return RefundResult{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return RefundResult{}, err
	}
	reason := req.Reason
	if reason == "" {
		reason = "refund"
	}

	var out RefundResult
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		res, err := lockReservation(ctx, tx, req.ReservationID)
		if err != nil {
			return err
		}
		switch res.Status {
		case StatusRefunded:
			entry, err := entryByReservationKind(ctx, tx, res.ID, "refund")
			if err != nil {
				return ledgerEntryReadError(err, res.ID, "refund")
			}
			if entry.HeldDelta != -int64(res.HoldMicros) {
				return fmt.Errorf("%w: refunded reservation %s disagrees with its ledger", ErrWalletState, res.ID)
			}
			out = RefundResult{
				ReservationID:  res.ID,
				EntryID:        entry.ID,
				UserID:         res.UserID,
				ReleasedMicros: res.HoldMicros,
				BalanceMicros:  pricing.Micros(entry.BalanceAfter),
				Replayed:       true,
			}
			return nil
		case StatusSettled:
			return fmt.Errorf("%w: %s", ErrReservationSettled, res.ID)
		case StatusHeld:
		default:
			return fmt.Errorf("%w: reservation %s has status %q", ErrWalletState, res.ID, res.Status)
		}

		balance, held, err := lockWallet(ctx, tx, res.UserID)
		if err != nil {
			return err
		}
		if int64(res.HoldMicros) > held {
			return fmt.Errorf("%w: reservation %s holds %d but wallet %s holds %d",
				ErrWalletState, res.ID, res.HoldMicros, res.UserID, held)
		}
		newHeld, err := subInt64(held, int64(res.HoldMicros))
		if err != nil {
			return fmt.Errorf("%w: reservation %s release: %v", ErrWalletState, res.ID, err)
		}
		entryID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate refund entry id: %w", err)
		}
		if err := insertLedgerEntry(ctx, tx, ledgerEntry{
			ID:             entryID,
			UserID:         res.UserID,
			ReservationID:  res.ID,
			Kind:           "refund",
			BalanceDelta:   0,
			HeldDelta:      -int64(res.HoldMicros),
			BalanceAfter:   balance,
			HeldAfter:      newHeld,
			IdempotencyKey: internalIDKeyPrefix + res.ID.String() + ":refund",
			Description:    reason,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		if err := updateWallet(ctx, tx, res.UserID, balance, newHeld, now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE billing_reservations
			 SET status = 'refunded', refund_entry_id = $2::uuid,
			     refunded_at = $3, updated_at = $3
			 WHERE id = $1::uuid AND status = 'held'`,
			res.ID.String(), entryID.String(), now)
		if err != nil {
			return fmt.Errorf("wallet: mark reservation refunded: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: reservation %s changed while refunding", ErrWalletState, res.ID)
		}
		out = RefundResult{
			ReservationID:  res.ID,
			EntryID:        entryID,
			UserID:         res.UserID,
			ReleasedMicros: res.HoldMicros,
			BalanceMicros:  pricing.Micros(balance),
		}
		return nil
	})
	if err != nil {
		return RefundResult{}, err
	}
	return out, nil
}

// RecoverStaleHolds refunds held reservations whose expires_at is at or before
// cutoff, up to limit of them, and returns what this call released. A limit
// above maxRecoveryBatch is lowered to it, so one call has a bound.
//
// It reads the candidate ids first, then refunds each in its own transaction.
// A reservation settled or refunded by another process in between is skipped
// and not counted. Partial progress is committed; an error returns the work
// already released alongside the error, so a caller may retry the rest.
func (s *Service) RecoverStaleHolds(ctx context.Context, cutoff time.Time, limit int) (StaleRecovery, error) {
	if err := s.usable(); err != nil {
		return StaleRecovery{}, err
	}
	if cutoff.IsZero() {
		return StaleRecovery{}, fmt.Errorf("%w: stale cutoff is the zero time", ErrInvalidRequest)
	}
	if limit <= 0 {
		return StaleRecovery{}, fmt.Errorf("%w: stale limit must be positive", ErrInvalidRequest)
	}
	if limit > maxRecoveryBatch {
		limit = maxRecoveryBatch
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM billing_reservations
		 WHERE status = 'held' AND expires_at <= $1
		 ORDER BY expires_at, id
		 LIMIT $2`,
		cutoff.UTC(), limit)
	if err != nil {
		return StaleRecovery{}, fmt.Errorf("wallet: list stale reservations: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			rows.Close()
			return StaleRecovery{}, fmt.Errorf("wallet: list stale reservations: %w", err)
		}
		id, err := uuid.Parse(text)
		if err != nil {
			rows.Close()
			return StaleRecovery{}, fmt.Errorf("%w: stale reservation id %q", ErrWalletState, text)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StaleRecovery{}, fmt.Errorf("wallet: list stale reservations: %w", err)
	}

	var out StaleRecovery
	for _, id := range ids {
		result, err := s.Refund(ctx, RefundRequest{ReservationID: id, Reason: "stale hold"})
		switch {
		case err == nil:
			if result.Replayed {
				// Another process released it between the list and the
				// refund; this pass released no money.
				continue
			}
			out.Reservations++
			released, overflow := addInt64(int64(out.ReleasedMicros), int64(result.ReleasedMicros))
			if overflow != nil {
				return out, fmt.Errorf("wallet: released total: %w", overflow)
			}
			out.ReleasedMicros = pricing.Micros(released)
		case errors.Is(err, ErrReservationSettled), errors.Is(err, ErrReservationRefunded):
			continue
		default:
			return out, fmt.Errorf("wallet: recover stale reservation %s: %w", id, err)
		}
	}
	return out, nil
}

func (s *Service) validateReserve(req ReserveRequest) error {
	if err := validateUserID(req.UserID); err != nil {
		return err
	}
	if err := validateRequiredText("request id", req.RequestID, MaxRequestIDLength); err != nil {
		return err
	}
	if req.TTL < 0 {
		return fmt.Errorf("%w: hold TTL %s is negative", ErrInvalidRequest, req.TTL)
	}
	if err := validateOptionalText("description", req.Description, MaxDescriptionLength); err != nil {
		return err
	}
	if err := req.Quote.Validate(); err != nil {
		return fmt.Errorf("wallet: reserve quote: %w", err)
	}
	if err := req.EstimatedUsage.Validate(); err != nil {
		return fmt.Errorf("wallet: reserve estimated usage: %w", err)
	}
	return nil
}

func ledgerEntryReadError(err error, reservationID uuid.UUID, kind string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: reservation %s has no %s entry", ErrWalletState, reservationID, kind)
	}
	return err
}

func unusedHold(hold, charge pricing.Micros) pricing.Micros {
	if charge < hold {
		return hold - charge
	}
	return 0
}
