package wallet

import (
	"context"
	"fmt"
	"time"
)

// StaleReservations returns expired held reservations for an application
// reconciler. It does not refund them: a crashed process may have recorded
// billable usage that the reconciler must settle first.
func (s *Service) StaleReservations(ctx context.Context, cutoff time.Time, limit int) ([]Reservation, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if cutoff.IsZero() || limit <= 0 {
		return nil, fmt.Errorf("%w: stale cutoff and limit are required", ErrInvalidRequest)
	}
	if limit > maxRecoveryBatch {
		limit = maxRecoveryBatch
	}
	rows, err := s.pool.Query(ctx, `SELECT `+reservationColumns+` FROM billing_reservations
		WHERE status = 'held' AND expires_at <= $1 ORDER BY expires_at, id LIMIT $2`, cutoff.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
