package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

// Credit adds amountMicros to userID's wallet. It is safe for a payment
// callback to call it again with the same idempotencyKey, from any Service or
// process: the first call records the key and the later calls return its
// result with Replayed = true, adding nothing. A replay reports the first
// call's amount even when the retry names a different one; the first call
// wins.
//
// The transaction locks the wallet before looking the key up, so two replays
// for one user run one after the other. A key already recorded for another
// user, or for a reservation operation, is refused with
// ErrIdempotencyConflict rather than credited to the wrong wallet.
func (s *Service) Credit(ctx context.Context, userID uuid.UUID, amountMicros int64, idempotencyKey, description string) (CreditResult, error) {
	if err := s.usable(); err != nil {
		return CreditResult{}, err
	}
	if err := validateUserID(userID); err != nil {
		return CreditResult{}, err
	}
	if amountMicros <= 0 {
		return CreditResult{}, fmt.Errorf("%w: credit amount must be positive, got %d", ErrInvalidRequest, amountMicros)
	}
	if err := validateCreditKey(idempotencyKey); err != nil {
		return CreditResult{}, err
	}
	if err := validateOptionalText("description", description, MaxDescriptionLength); err != nil {
		return CreditResult{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return CreditResult{}, err
	}

	var out CreditResult
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		balance, held, err := ensureAndLockWallet(ctx, tx, userID, now)
		if err != nil {
			return err
		}
		existing, err := entryByIDKey(ctx, tx, idempotencyKey)
		if err == nil {
			if existing.UserID != userID || existing.Kind != "credit" {
				return fmt.Errorf("%w: key %q is a %s for user %s",
					ErrIdempotencyConflict, idempotencyKey, existing.Kind, existing.UserID)
			}
			out = CreditResult{
				EntryID:       existing.ID,
				UserID:        existing.UserID,
				AmountMicros:  pricing.Micros(existing.BalanceDelta),
				BalanceMicros: pricing.Micros(existing.BalanceAfter),
				Replayed:      true,
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("wallet: read credit key %q: %w", idempotencyKey, err)
		}

		newBalance, err := addInt64(balance, amountMicros)
		if err != nil {
			return fmt.Errorf("wallet: credit %s: %w", userID, err)
		}
		entryID, err := uuid.NewRandom()
		if err != nil {
			return fmt.Errorf("wallet: generate credit entry id: %w", err)
		}
		if err := insertLedgerEntry(ctx, tx, ledgerEntry{
			ID:             entryID,
			UserID:         userID,
			Kind:           "credit",
			BalanceDelta:   amountMicros,
			HeldDelta:      0,
			BalanceAfter:   newBalance,
			HeldAfter:      held,
			IdempotencyKey: idempotencyKey,
			Description:    description,
			CreatedAt:      now,
		}); err != nil {
			return err
		}
		if err := updateWallet(ctx, tx, userID, newBalance, held, now); err != nil {
			return err
		}
		out = CreditResult{
			EntryID:       entryID,
			UserID:        userID,
			AmountMicros:  pricing.Micros(amountMicros),
			BalanceMicros: pricing.Micros(newBalance),
		}
		return nil
	})
	if err != nil {
		return CreditResult{}, err
	}
	return out, nil
}
