// Package pricing holds house-channel prices and turns token counts into
// money without floating point.
//
// Money is int64 micro-CNY: one CNY is 1,000,000 micros. Token prices are
// micro-CNY per million tokens. A zero price is a known free price; a missing
// price is unknown and fails closed with ErrUnknownPrice, so an unpriced
// model is never served as if it were free.
//
// Table is an in-memory price list. The durable price rows live in the
// house_channel_prices table in
// internal/cloud/store/migrations/002_billing.sql; a deployment maps those
// rows into a Table when it builds its gateway configuration.
package pricing

import (
	"errors"
	"fmt"
	"math"
)

const (
	// CurrencyCNY is the only currency the house channel bills in.
	CurrencyCNY = "CNY"

	// MicrosPerCNY is the number of micro-CNY in one CNY.
	MicrosPerCNY int64 = 1_000_000

	// TokensPerMillion is the denominator of every per-million-token rate.
	TokensPerMillion int64 = 1_000_000
)

var (
	// ErrUnknownPrice means no price was found for a key. Callers must not
	// substitute zero: an unknown price fails closed.
	ErrUnknownPrice = errors.New("pricing: unknown price")

	// ErrInvalidKey means a price key is missing its provider or model.
	ErrInvalidKey = errors.New("pricing: invalid price key")

	// ErrInvalidPrice means a price has a negative rate or an unusable unit.
	ErrInvalidPrice = errors.New("pricing: invalid price")

	// ErrInvalidUsage means a usage count is negative or otherwise unusable.
	ErrInvalidUsage = errors.New("pricing: invalid usage")

	// ErrInvalidQuote means a quote is incomplete, for example when its
	// currency is empty. A zero-value quote is never a free quote.
	ErrInvalidQuote = errors.New("pricing: invalid quote")

	// ErrOverflow means the exact cost does not fit in an int64.
	ErrOverflow = errors.New("pricing: amount overflows int64")
)

// Micros is an amount of money in micro-CNY. It is persisted as a bigint.
type Micros int64

// Int64 returns the amount as a plain int64 for drivers and storage.
func (m Micros) Int64() int64 { return int64(m) }

// String renders the amount for messages. It is not a machine format.
func (m Micros) String() string { return fmt.Sprintf("%d micro-CNY", int64(m)) }

// Usage is one call's token counts. The zero value is a call that used
// nothing; it is valid.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// Validate refuses negative token counts.
func (u Usage) Validate() error {
	for _, f := range []struct {
		name string
		n    int64
	}{
		{"input tokens", u.InputTokens},
		{"output tokens", u.OutputTokens},
		{"cache read tokens", u.CacheReadTokens},
		{"cache write tokens", u.CacheWriteTokens},
	} {
		if f.n < 0 {
			return fmt.Errorf("%w: %s is %d", ErrInvalidUsage, f.name, f.n)
		}
	}
	return nil
}

// Price is a house-channel price. Each token rate is micro-CNY per million
// tokens; FixedPerCall is micro-CNY per call. A zero rate is an explicit
// free component, not an unknown price.
type Price struct {
	InputPerMTok      Micros `json:"input_micros_per_mtok"`
	OutputPerMTok     Micros `json:"output_micros_per_mtok"`
	CacheReadPerMTok  Micros `json:"cache_read_micros_per_mtok"`
	CacheWritePerMTok Micros `json:"cache_write_micros_per_mtok"`
	FixedPerCall      Micros `json:"fixed_per_call_micros"`
}

// Validate refuses a negative rate. Every field is required; an explicit
// zero is allowed.
func (p Price) Validate() error {
	for _, f := range []struct {
		name string
		v    Micros
	}{
		{"input per million tokens", p.InputPerMTok},
		{"output per million tokens", p.OutputPerMTok},
		{"cache read per million tokens", p.CacheReadPerMTok},
		{"cache write per million tokens", p.CacheWritePerMTok},
		{"fixed per call", p.FixedPerCall},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: %s is %d", ErrInvalidPrice, f.name, f.v)
		}
	}
	return nil
}

// Cost returns the charge for u at p, in micro-CNY. The exact value is
// rounded half up to the next micro; components are summed before the
// single division, so a small component cannot round up on its own. A price
// whose exact cost is below half a micro charges zero for that call.
//
// The arithmetic is int64 throughout and refuses an overflow with
// ErrOverflow instead of wrapping into a wrong charge.
func (p Price) Cost(u Usage) (Micros, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if err := u.Validate(); err != nil {
		return 0, err
	}

	// numerator is the exact cost scaled by TokensPerMillion, so the whole
	// sum is divided once at the end.
	var numerator int64
	for _, component := range []struct {
		name   string
		tokens int64
		rate   Micros
	}{
		{"input", u.InputTokens, p.InputPerMTok},
		{"output", u.OutputTokens, p.OutputPerMTok},
		{"cache read", u.CacheReadTokens, p.CacheReadPerMTok},
		{"cache write", u.CacheWriteTokens, p.CacheWritePerMTok},
	} {
		if err := addProduct(&numerator, component.tokens, int64(component.rate)); err != nil {
			return 0, fmt.Errorf("%w: %s cost", err, component.name)
		}
	}
	if p.FixedPerCall != 0 {
		if err := addProduct(&numerator, int64(p.FixedPerCall), TokensPerMillion); err != nil {
			return 0, fmt.Errorf("%w: fixed per-call cost", err)
		}
	}

	whole := numerator / TokensPerMillion
	remainder := numerator % TokensPerMillion
	if remainder >= (TokensPerMillion+1)/2 {
		whole++
	}
	return Micros(whole), nil
}

// addProduct adds tokens*rate to *acc. Both factors and the accumulator are
// non-negative in every caller, so the checks can stay simple.
func addProduct(acc *int64, tokens, rate int64) error {
	if tokens == 0 || rate == 0 {
		return nil
	}
	if tokens < 0 {
		return ErrInvalidUsage
	}
	if rate < 0 {
		return ErrInvalidPrice
	}
	if tokens > math.MaxInt64/rate {
		return ErrOverflow
	}
	product := tokens * rate
	if *acc > math.MaxInt64-product {
		return ErrOverflow
	}
	*acc += product
	return nil
}
