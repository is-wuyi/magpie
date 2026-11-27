package wallet

import (
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/cloud/pricing"
)

// Status is a reservation's lifecycle state.
type Status string

const (
	// StatusHeld means the hold is active and the reservation can still be
	// settled or refunded.
	StatusHeld Status = "held"

	// StatusSettled means actual usage was charged and the hold released.
	StatusSettled Status = "settled"

	// StatusRefunded means the hold was released with no charge.
	StatusRefunded Status = "refunded"
)

// Balance is one user's stored money. AvailableMicros is BalanceMicros minus
// HeldMicros and may be negative after a settlement overdraws the wallet.
type Balance struct {
	UserID          uuid.UUID
	BalanceMicros   pricing.Micros
	HeldMicros      pricing.Micros
	AvailableMicros pricing.Micros
}

// ReserveRequest asks for one request's hold. Quote must come from a price
// lookup or an explicit FreeQuote; a zero-value Quote is refused rather than
// treated as free. RequestID is the caller's idempotency key: the same
// (UserID, RequestID) always returns the first reservation, so a retry cannot
// take a second hold. TTL zero means Service.defaultHoldTTL.
type ReserveRequest struct {
	UserID         uuid.UUID
	RequestID      string
	Quote          pricing.Quote
	EstimatedUsage pricing.Usage
	TTL            time.Duration
	Description    string
}

// Reservation is a hold as stored. Snapshot is the price frozen at reserve
// time; EstimatedUsage is what the hold was sized for.
type Reservation struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	RequestID      string
	Status         Status
	Snapshot       pricing.Snapshot
	EstimatedUsage pricing.Usage
	HoldMicros     pricing.Micros
	// ChargedMicros is zero while the reservation is still held.
	ChargedMicros pricing.Micros
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Replayed is true when the call returned an existing reservation
	// instead of creating one.
	Replayed bool
}

// SettleRequest charges a reservation. ActualUsage is priced at the
// reservation's snapshot, never at a current table.
type SettleRequest struct {
	ReservationID uuid.UUID
	ActualUsage   pricing.Usage
	Description   string
}

// Settlement is what a settle did or, on a replay, what the first settle did.
type Settlement struct {
	ReservationID uuid.UUID
	EntryID       uuid.UUID
	UserID        uuid.UUID
	ChargedMicros pricing.Micros
	// ReleasedMicros is the whole hold released by this settlement.
	ReleasedMicros pricing.Micros
	// UnusedMicros is the part of the hold the charge did not use:
	// max(hold-charge, 0).
	UnusedMicros  pricing.Micros
	BalanceMicros pricing.Micros
	Replayed      bool
}

// RefundRequest releases a held reservation without charging it.
type RefundRequest struct {
	ReservationID uuid.UUID
	Reason        string
}

// RefundResult is what a refund did or, on a replay, what the first refund
// did.
type RefundResult struct {
	ReservationID  uuid.UUID
	EntryID        uuid.UUID
	UserID         uuid.UUID
	ReleasedMicros pricing.Micros
	BalanceMicros  pricing.Micros
	Replayed       bool
}

// CreditResult is what a credit did or, on a replay, what the first credit
// did. BalanceMicros is the balance after that entry, so a replay reports the
// same number as the first call.
type CreditResult struct {
	EntryID       uuid.UUID      `json:"entryId"`
	UserID        uuid.UUID      `json:"userId"`
	AmountMicros  pricing.Micros `json:"amountMicros"`
	BalanceMicros pricing.Micros `json:"balanceMicros"`
	Replayed      bool           `json:"replayed"`
}

// StaleRecovery reports one RecoverStaleHolds pass.
type StaleRecovery struct {
	// Reservations is how many holds this call released. Holds another
	// process released first are not counted.
	Reservations int
	// ReleasedMicros is the money those holds released.
	ReleasedMicros pricing.Micros
}
