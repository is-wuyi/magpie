package channels

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/yetone/magpie/internal/cloud/pricing"
)

type HousePrice struct {
	Channel       string        `json:"channel"`
	Provider      string        `json:"provider"`
	Model         string        `json:"model"`
	Price         pricing.Price `json:"price"`
	EffectiveFrom time.Time     `json:"effectiveFrom"`
}

func (s *Service) Quote(ctx context.Context, channelID, providerID, model string) (pricing.Quote, error) {
	key := pricing.Key{Channel: channelID, Provider: providerID, Model: model}
	if _, err := uuid.Parse(channelID); err != nil {
		return pricing.Quote{}, err
	}
	var p pricing.Price
	err := s.db.QueryRow(ctx, `SELECT input_micros_per_mtok, output_micros_per_mtok,
		cache_read_micros_per_mtok, cache_write_micros_per_mtok, fixed_per_call_micros
		FROM house_channel_prices WHERE channel = $1 AND provider = $2 AND model = $3 AND effective_to IS NULL`,
		channelID, providerID, model).Scan(&p.InputPerMTok, &p.OutputPerMTok, &p.CacheReadPerMTok, &p.CacheWritePerMTok, &p.FixedPerCall)
	if errors.Is(err, pgx.ErrNoRows) {
		return pricing.Quote{}, pricing.ErrUnknownPrice
	}
	if err != nil {
		return pricing.Quote{}, err
	}
	return pricing.NewQuote(key, p)
}

func (s *Service) SetHousePrice(ctx context.Context, channelID uuid.UUID, providerID, model string, price pricing.Price) error {
	if err := price.Validate(); err != nil {
		return err
	}
	providerID, model = strings.TrimSpace(providerID), strings.TrimSpace(model)
	if providerID == "" || model == "" {
		return pricing.ErrInvalidKey
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM providers WHERE public_id = $1 AND house_channel_id = $2)`, providerID, channelID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrNotFound
	}
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE house_channel_prices SET effective_to = $4, updated_at = $4
		WHERE channel = $1 AND provider = $2 AND model = $3 AND effective_to IS NULL`, channelID.String(), providerID, model, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO house_channel_prices
		(id, channel, provider, model, input_micros_per_mtok, output_micros_per_mtok,
		cache_read_micros_per_mtok, cache_write_micros_per_mtok, fixed_per_call_micros, effective_from, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`, uuid.New(), channelID.String(), providerID, model,
		price.InputPerMTok, price.OutputPerMTok, price.CacheReadPerMTok, price.CacheWritePerMTok, price.FixedPerCall, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ListHousePrices(ctx context.Context, channelID uuid.UUID) ([]HousePrice, error) {
	rows, err := s.db.Query(ctx, `SELECT provider, model, input_micros_per_mtok, output_micros_per_mtok,
		cache_read_micros_per_mtok, cache_write_micros_per_mtok, fixed_per_call_micros, effective_from
		FROM house_channel_prices WHERE channel = $1 AND effective_to IS NULL ORDER BY provider, model`, channelID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HousePrice{}
	for rows.Next() {
		var p HousePrice
		p.Channel = channelID.String()
		if err := rows.Scan(&p.Provider, &p.Model, &p.Price.InputPerMTok, &p.Price.OutputPerMTok,
			&p.Price.CacheReadPerMTok, &p.Price.CacheWritePerMTok, &p.Price.FixedPerCall, &p.EffectiveFrom); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
