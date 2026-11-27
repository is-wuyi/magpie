package pricing

import (
	"fmt"
	"strings"
	"time"
)

// Key names what a price is for: a house channel (optional), a provider and
// a model. Keys match exactly; nothing is lower-cased or trimmed, so two
// different model ids never share a price by accident.
type Key struct {
	Channel  string `json:"channel,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Validate refuses a key with no provider or model.
func (k Key) Validate() error {
	if strings.TrimSpace(k.Provider) == "" {
		return fmt.Errorf("%w: provider is empty", ErrInvalidKey)
	}
	if strings.TrimSpace(k.Model) == "" {
		return fmt.Errorf("%w: model is empty", ErrInvalidKey)
	}
	if k.Channel != "" && strings.TrimSpace(k.Channel) == "" {
		return fmt.Errorf("%w: channel is blank", ErrInvalidKey)
	}
	return nil
}

// String renders the key for errors and logs.
func (k Key) String() string {
	if k.Channel != "" {
		return k.Channel + "/" + k.Provider + "/" + k.Model
	}
	return k.Provider + "/" + k.Model
}

// Table is an immutable price list. Lookup is safe from several goroutines
// after NewTable returns.
type Table struct {
	prices map[Key]Price
}

// NewTable validates every entry and returns a read-only table. A nil or
// empty map is a valid table in which every price is unknown.
func NewTable(prices map[Key]Price) (*Table, error) {
	t := &Table{prices: make(map[Key]Price, len(prices))}
	for key, price := range prices {
		if err := key.Validate(); err != nil {
			return nil, err
		}
		if err := price.Validate(); err != nil {
			return nil, fmt.Errorf("pricing: table entry %s: %w", key, err)
		}
		t.prices[key] = price
	}
	return t, nil
}

// Lookup returns the quote for key. A missing key is ErrUnknownPrice; it is
// never a zero price. A stored zero price is returned as a valid free quote.
func (t *Table) Lookup(key Key) (Quote, error) {
	if err := key.Validate(); err != nil {
		return Quote{}, err
	}
	if t == nil {
		return Quote{}, fmt.Errorf("%w: %s", ErrUnknownPrice, key)
	}
	price, ok := t.prices[key]
	if !ok {
		return Quote{}, fmt.Errorf("%w: %s", ErrUnknownPrice, key)
	}
	return Quote{Key: key, Price: price, Currency: CurrencyCNY}, nil
}

// Quote is a validated price for one key. A zero-value Quote is not free:
// its missing currency makes Validate fail, so a caller that forgot to look
// a price up cannot reserve at zero by accident.
type Quote struct {
	Key      Key    `json:"key"`
	Price    Price  `json:"price"`
	Currency string `json:"currency"`
}

// NewQuote validates key and price and builds a CNY quote. An all-zero
// price is allowed and means the call is free.
func NewQuote(key Key, price Price) (Quote, error) {
	if err := key.Validate(); err != nil {
		return Quote{}, err
	}
	if err := price.Validate(); err != nil {
		return Quote{}, err
	}
	return Quote{Key: key, Price: price, Currency: CurrencyCNY}, nil
}

// FreeQuote is an explicit zero price for key.
func FreeQuote(key Key) (Quote, error) {
	return NewQuote(key, Price{})
}

// Validate checks that the quote is complete and in the supported currency.
func (q Quote) Validate() error {
	if err := q.Key.Validate(); err != nil {
		return err
	}
	if err := q.Price.Validate(); err != nil {
		return err
	}
	if q.Currency != CurrencyCNY {
		return fmt.Errorf("%w: currency %q is not %s", ErrInvalidQuote, q.Currency, CurrencyCNY)
	}
	return nil
}

// Cost prices usage at the quoted rates.
func (q Quote) Cost(u Usage) (Micros, error) {
	if err := q.Validate(); err != nil {
		return 0, err
	}
	return q.Price.Cost(u)
}

// Snapshot is a quote frozen at the moment a reservation was made. It is
// persisted as JSON in billing_reservations and usage_records, so a later
// price change cannot reprice an in-flight call.
type Snapshot struct {
	Quote
	CapturedAt time.Time `json:"captured_at"`
}

// NewSnapshot validates q and records when it was captured.
func NewSnapshot(q Quote, at time.Time) (Snapshot, error) {
	if err := q.Validate(); err != nil {
		return Snapshot{}, err
	}
	if at.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: capture time is zero", ErrInvalidQuote)
	}
	return Snapshot{Quote: q, CapturedAt: at.UTC()}, nil
}

// Validate checks a snapshot read back from storage.
func (s Snapshot) Validate() error {
	if err := s.Quote.Validate(); err != nil {
		return err
	}
	if s.CapturedAt.IsZero() {
		return fmt.Errorf("%w: capture time is zero", ErrInvalidQuote)
	}
	return nil
}

// Cost prices usage at the captured rates.
func (s Snapshot) Cost(u Usage) (Micros, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	return s.Price.Cost(u)
}
