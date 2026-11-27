package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestPriceCostComponents(t *testing.T) {
	p := Price{
		InputPerMTok:      3_000_000,
		OutputPerMTok:     15_000_000,
		CacheReadPerMTok:  300_000,
		CacheWritePerMTok: 3_750_000,
		FixedPerCall:      1_000,
	}
	u := Usage{
		InputTokens:      1_000,
		OutputTokens:     500,
		CacheReadTokens:  2_000,
		CacheWriteTokens: 1_000,
	}
	got, err := p.Cost(u)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 3,000 + 7,500 + 600 + 3,750 + 1,000 micro-CNY.
	if want := Micros(15_850); got != want {
		t.Fatalf("Cost = %d, want %d", got, want)
	}
}

func TestPriceCostEachComponentAlone(t *testing.T) {
	const million = 1_000_000
	tests := []struct {
		name  string
		price Price
		usage Usage
		want  Micros
	}{
		{
			name:  "input",
			price: Price{InputPerMTok: 3_000_000},
			usage: Usage{InputTokens: million},
			want:  3_000_000,
		},
		{
			name:  "output",
			price: Price{OutputPerMTok: 15_000_000},
			usage: Usage{OutputTokens: million},
			want:  15_000_000,
		},
		{
			name:  "cache read",
			price: Price{CacheReadPerMTok: 300_000},
			usage: Usage{CacheReadTokens: million},
			want:  300_000,
		},
		{
			name:  "cache write",
			price: Price{CacheWritePerMTok: 3_750_000},
			usage: Usage{CacheWriteTokens: million},
			want:  3_750_000,
		},
		{
			name:  "fixed per call",
			price: Price{FixedPerCall: 1_234},
			usage: Usage{},
			want:  1_234,
		},
		{
			name:  "fixed per call does not scale with tokens",
			price: Price{FixedPerCall: 1_234},
			usage: Usage{InputTokens: 10 * million, OutputTokens: 10 * million},
			want:  1_234,
		},
		{
			name:  "zero price is free at any usage",
			price: Price{},
			usage: Usage{InputTokens: 123, OutputTokens: 456, CacheReadTokens: 789, CacheWriteTokens: 10},
			want:  0,
		},
		{
			name:  "zero usage has no variable cost",
			price: Price{InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000},
			usage: Usage{},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.price.Cost(tt.usage)
			if err != nil {
				t.Fatalf("Cost: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Cost = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPriceCostRoundsHalfUpOnce(t *testing.T) {
	// A rate of 1 micro-CNY per million tokens makes one token worth
	// 1/1,000,000 micro-CNY, so the numerator is the token count.
	p := Price{InputPerMTok: 1}
	tests := []struct {
		tokens int64
		want   Micros
	}{
		{0, 0},
		{1, 0},
		{499_999, 0},
		{500_000, 1},
		{500_001, 1},
		{999_999, 1},
		{1_000_000, 1},
		{1_000_001, 1},
		{1_499_999, 1},
		{1_500_000, 2},
		{2_500_000, 3},
	}
	for _, tt := range tests {
		got, err := p.Cost(Usage{InputTokens: tt.tokens})
		if err != nil {
			t.Fatalf("Cost(%d tokens): %v", tt.tokens, err)
		}
		if got != tt.want {
			t.Fatalf("Cost(%d tokens) = %d, want %d", tt.tokens, got, tt.want)
		}
	}
}

func TestPriceCostSumsBeforeRounding(t *testing.T) {
	// Each component alone is 0.4 micro-CNY; together they are 0.8, which
	// rounds to 1. Rounding each component first would charge 0.
	p := Price{InputPerMTok: 1, OutputPerMTok: 1}
	got, err := p.Cost(Usage{InputTokens: 400_000, OutputTokens: 400_000})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if want := Micros(1); got != want {
		t.Fatalf("Cost = %d, want %d", got, want)
	}
}

func TestPriceCostNearMaxInt64(t *testing.T) {
	// The largest rate that still multiplies by one million tokens without
	// overflowing, and the same bound for a fixed per-call charge.
	rate := Micros(math.MaxInt64 / TokensPerMillion)
	got, err := (Price{InputPerMTok: rate}).Cost(Usage{InputTokens: TokensPerMillion})
	if err != nil {
		t.Fatalf("input Cost: %v", err)
	}
	if want := Micros(int64(rate)); got != want {
		t.Fatalf("input Cost = %d, want %d", got, want)
	}
	got, err = (Price{FixedPerCall: rate}).Cost(Usage{})
	if err != nil {
		t.Fatalf("fixed Cost: %v", err)
	}
	if want := Micros(int64(rate)); got != want {
		t.Fatalf("fixed Cost = %d, want %d", got, want)
	}
}

func TestPriceCostRejectsNegativeInput(t *testing.T) {
	if _, err := (Price{InputPerMTok: -1}).Cost(Usage{}); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("negative price error = %v, want ErrInvalidPrice", err)
	}
	if _, err := (Price{}).Cost(Usage{OutputTokens: -1}); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("negative usage error = %v, want ErrInvalidUsage", err)
	}
	for _, p := range []Price{
		{OutputPerMTok: -1},
		{CacheReadPerMTok: -1},
		{CacheWritePerMTok: -1},
		{FixedPerCall: -1},
	} {
		if _, err := p.Cost(Usage{}); !errors.Is(err, ErrInvalidPrice) {
			t.Fatalf("price %+v error = %v, want ErrInvalidPrice", p, err)
		}
	}
	for _, u := range []Usage{
		{InputTokens: -1},
		{OutputTokens: -1},
		{CacheReadTokens: -1},
		{CacheWriteTokens: -1},
	} {
		if _, err := (Price{}).Cost(u); !errors.Is(err, ErrInvalidUsage) {
			t.Fatalf("usage %+v error = %v, want ErrInvalidUsage", u, err)
		}
	}
}

func TestPriceCostRefusesOverflow(t *testing.T) {
	tests := []struct {
		name  string
		price Price
		usage Usage
	}{
		{
			name:  "product",
			price: Price{InputPerMTok: math.MaxInt64/2 + 1},
			usage: Usage{InputTokens: 2},
		},
		{
			name:  "sum of products",
			price: Price{InputPerMTok: math.MaxInt64 / 3, OutputPerMTok: 1},
			usage: Usage{InputTokens: 3, OutputTokens: 3},
		},
		{
			name:  "fixed product",
			price: Price{FixedPerCall: Micros(math.MaxInt64/MicrosPerCNY + 1)},
			usage: Usage{},
		},
		{
			name:  "fixed added to product",
			price: Price{InputPerMTok: math.MaxInt64 - 500_000, FixedPerCall: 1},
			usage: Usage{InputTokens: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.price.Cost(tt.usage); !errors.Is(err, ErrOverflow) {
				t.Fatalf("Cost error = %v, want ErrOverflow", err)
			}
		})
	}
}

func TestKeyValidate(t *testing.T) {
	valid := []Key{
		{Provider: "openai", Model: "gpt-5"},
		{Channel: "house", Provider: "openai", Model: "gpt-5"},
		{Channel: "house", Provider: "openai", Model: "gpt-5/2026"},
	}
	for _, k := range valid {
		if err := k.Validate(); err != nil {
			t.Fatalf("Key(%+v).Validate = %v", k, err)
		}
	}
	invalid := []Key{
		{},
		{Model: "gpt-5"},
		{Provider: "openai"},
		{Provider: " ", Model: "gpt-5"},
		{Provider: "openai", Model: " "},
		{Channel: " ", Provider: "openai", Model: "gpt-5"},
	}
	for _, k := range invalid {
		if err := k.Validate(); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Key(%+v).Validate = %v, want ErrInvalidKey", k, err)
		}
	}
}

func TestTableLookupFailsClosed(t *testing.T) {
	key := Key{Provider: "openai", Model: "gpt-5"}
	free := Key{Provider: "openai", Model: "free-model"}
	table, err := NewTable(map[Key]Price{
		key:  {InputPerMTok: 3_000_000},
		free: {},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}

	quote, err := table.Lookup(key)
	if err != nil {
		t.Fatalf("Lookup(known): %v", err)
	}
	if quote.Currency != CurrencyCNY {
		t.Fatalf("currency = %q, want %q", quote.Currency, CurrencyCNY)
	}
	if quote.Price.InputPerMTok != 3_000_000 {
		t.Fatalf("input rate = %d", quote.Price.InputPerMTok)
	}

	freeQuote, err := table.Lookup(free)
	if err != nil {
		t.Fatalf("Lookup(free): %v", err)
	}
	if cost, err := freeQuote.Cost(Usage{InputTokens: 1_000_000}); err != nil || cost != 0 {
		t.Fatalf("free quote cost = %d, %v; want 0, nil", cost, err)
	}

	if _, err := table.Lookup(Key{Provider: "openai", Model: "other"}); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown key error = %v, want ErrUnknownPrice", err)
	}
	if _, err := table.Lookup(Key{}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty key error = %v, want ErrInvalidKey", err)
	}

	var nilTable *Table
	if _, err := nilTable.Lookup(key); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("nil table error = %v, want ErrUnknownPrice", err)
	}
}

func TestNewTableValidatesEveryEntry(t *testing.T) {
	if _, err := NewTable(map[Key]Price{
		{Provider: "openai", Model: "gpt-5"}: {InputPerMTok: -1},
	}); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("negative price error = %v, want ErrInvalidPrice", err)
	}
	if _, err := NewTable(map[Key]Price{
		{Model: "gpt-5"}: {},
	}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key error = %v, want ErrInvalidKey", err)
	}
}

func TestQuoteRequiresARealPrice(t *testing.T) {
	key := Key{Provider: "openai", Model: "gpt-5"}
	var zero Quote
	if err := zero.Validate(); err == nil {
		t.Fatal("a zero-value Quote must not validate")
	}
	if _, err := zero.Cost(Usage{}); err == nil {
		t.Fatal("a zero-value Quote must not price usage")
	}

	missingCurrency := Quote{Key: key, Price: Price{}}
	if err := missingCurrency.Validate(); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("missing currency error = %v, want ErrInvalidQuote", err)
	}
	wrongCurrency := Quote{Key: key, Price: Price{}, Currency: "USD"}
	if err := wrongCurrency.Validate(); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("wrong currency error = %v, want ErrInvalidQuote", err)
	}

	free, err := FreeQuote(key)
	if err != nil {
		t.Fatalf("FreeQuote: %v", err)
	}
	if err := free.Validate(); err != nil {
		t.Fatalf("free quote Validate: %v", err)
	}
	if cost, err := free.Cost(Usage{OutputTokens: 1_000_000}); err != nil || cost != 0 {
		t.Fatalf("free quote cost = %d, %v; want 0, nil", cost, err)
	}
	if _, err := NewQuote(key, Price{OutputPerMTok: -1}); !errors.Is(err, ErrInvalidPrice) {
		t.Fatalf("negative quote error = %v, want ErrInvalidPrice", err)
	}
	if _, err := NewQuote(Key{}, Price{}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty key quote error = %v, want ErrInvalidKey", err)
	}
}

func TestSnapshotJSONKeepsIntegerAmounts(t *testing.T) {
	key := Key{Channel: "house", Provider: "openai", Model: "gpt-5"}
	quote, err := NewQuote(key, Price{
		InputPerMTok:      3_000_000,
		OutputPerMTok:     15_000_000,
		CacheReadPerMTok:  300_000,
		CacheWritePerMTok: 3_750_000,
		FixedPerCall:      1_000,
	})
	if err != nil {
		t.Fatalf("NewQuote: %v", err)
	}
	captured := time.Date(2026, 11, 26, 12, 34, 56, 789, time.UTC)
	snapshot, err := NewSnapshot(quote, captured)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	assertNoFloatingPoint(t, decoded, "")

	var back Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped snapshot Validate: %v", err)
	}
	if !back.CapturedAt.Equal(captured) {
		t.Fatalf("captured_at = %s, want %s", back.CapturedAt, captured)
	}
	if back.Key != key {
		t.Fatalf("key = %+v, want %+v", back.Key, key)
	}
	got, err := back.Cost(Usage{InputTokens: 1_000})
	if err != nil {
		t.Fatalf("round-tripped Cost: %v", err)
	}
	// 3,000 for input plus the 1,000 fixed per call.
	if want := Micros(4_000); got != want {
		t.Fatalf("round-tripped Cost = %d, want %d", got, want)
	}
}

func assertNoFloatingPoint(t *testing.T, value any, path string) {
	t.Helper()
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			t.Fatalf("%s holds a non-integer number %q", path, v)
		}
	case map[string]any:
		for k, child := range v {
			assertNoFloatingPoint(t, child, path+"."+k)
		}
	case []any:
		for _, child := range v {
			assertNoFloatingPoint(t, child, path+"[]")
		}
	}
}

func TestSnapshotRequiresCaptureTime(t *testing.T) {
	key := Key{Provider: "openai", Model: "gpt-5"}
	free, err := FreeQuote(key)
	if err != nil {
		t.Fatalf("FreeQuote: %v", err)
	}
	if _, err := NewSnapshot(free, time.Time{}); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("zero capture time error = %v, want ErrInvalidQuote", err)
	}
	unset := Snapshot{Quote: free}
	if err := unset.Validate(); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("unset snapshot error = %v, want ErrInvalidQuote", err)
	}
	if _, err := unset.Cost(Usage{}); !errors.Is(err, ErrInvalidQuote) {
		t.Fatalf("unset snapshot Cost error = %v, want ErrInvalidQuote", err)
	}
}

func TestMicrosStrings(t *testing.T) {
	if got, want := Micros(1_500_000).String(), "1500000 micro-CNY"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if got, want := Micros(42).Int64(), int64(42); got != want {
		t.Fatalf("Int64 = %d, want %d", got, want)
	}
}
