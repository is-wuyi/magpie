package wallet

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/pricing"
)

type UsageRecord struct {
	RequestID        string         `json:"requestId"`
	ReservationID    uuid.UUID      `json:"reservationId"`
	Channel          string         `json:"channel"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	InputTokens      int64          `json:"inputTokens"`
	OutputTokens     int64          `json:"outputTokens"`
	CacheReadTokens  int64          `json:"cacheReadTokens"`
	CacheWriteTokens int64          `json:"cacheWriteTokens"`
	CostMicros       pricing.Micros `json:"costMicros"`
	CreatedAt        time.Time      `json:"createdAt"`
}

func (s *Service) ListUsage(ctx context.Context, userID uuid.UUID, limit, offset int) ([]UsageRecord, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT request_id, reservation_id, channel, provider, model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros, created_at
		FROM usage_records WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRecord{}
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(&r.RequestID, &r.ReservationID, &r.Channel, &r.Provider, &r.Model, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.CostMicros, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
