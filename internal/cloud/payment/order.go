package payment

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Provider is a payment provider this package can bill through.
type Provider string

const (
	// ProviderAlipay is Alipay's computer website payment (page pay).
	ProviderAlipay Provider = "alipay"

	// ProviderWeChat is WeChat Pay's API v3 Native payment.
	ProviderWeChat Provider = "wechat"
)

// Valid reports whether p is one of the supported providers.
func (p Provider) Valid() bool { return p == ProviderAlipay || p == ProviderWeChat }

// Status is a recharge order's lifecycle state.
type Status string

const (
	// StatusPending is an order the user may still pay.
	StatusPending Status = "pending"

	// StatusSuccess means the provider confirmed the payment. Only a pending
	// order can reach it, and it is reached once.
	StatusSuccess Status = "success"

	// StatusFailed means the provider closed or refused the payment.
	StatusFailed Status = "failed"

	// StatusExpired means the deployment expired the order after closing it
	// at the provider. A payment that arrives afterwards is late.
	StatusExpired Status = "expired"
)

// CreditState is whether the wallet credit for a success order is still owed.
type CreditState string

const (
	// CreditNone means no credit is owed: the order is not successful yet,
	// or it failed before paying.
	CreditNone CreditState = "none"

	// CreditPending means the provider confirmed the payment but the wallet
	// credit has not been recorded yet. A callback or retry must settle it.
	CreditPending CreditState = "pending"

	// CreditCredited means the wallet ledger recorded the credit. It is
	// terminal for the order's credit.
	CreditCredited CreditState = "credited"
)

// PaymentState is a provider notification's meaning for the order.
type PaymentState string

const (
	// PaymentSucceeded means the provider says the money arrived.
	PaymentSucceeded PaymentState = "succeeded"

	// PaymentWaiting means the provider says the payment has not happened
	// yet; the order stays pending.
	PaymentWaiting PaymentState = "waiting"

	// PaymentClosed means the provider says the payment is closed.
	PaymentClosed PaymentState = "closed"

	// PaymentFailed means the provider says the payment attempt failed.
	PaymentFailed PaymentState = "failed"
)

// validPaymentState reports whether s is a known notification state.
func validPaymentState(s PaymentState) bool {
	switch s {
	case PaymentSucceeded, PaymentWaiting, PaymentClosed, PaymentFailed:
		return true
	}
	return false
}

// Preset is one server-controlled recharge amount. The client chooses the id;
// the server takes the amount and label from here, never from the request.
type Preset struct {
	ID        string `json:"id"`
	AmountFen Fen    `json:"amount_fen"`
	Label     string `json:"label"`
}

// presetSet is a validated, immutable list of presets.
type presetSet struct {
	list []Preset
	byID map[string]Preset
}

// newPresetSet validates the deployment's presets. Duplicate ids, unusable
// labels and amounts that are zero, negative or above MaxAmountFen are
// refused rather than skipped: a deployment that misconfigured a price must
// notice at startup.
func newPresetSet(presets []Preset) (*presetSet, error) {
	if len(presets) == 0 {
		return nil, fmt.Errorf("%w: no amount presets", ErrInvalidConfig)
	}
	set := &presetSet{
		list: make([]Preset, 0, len(presets)),
		byID: make(map[string]Preset, len(presets)),
	}
	for _, p := range presets {
		if !validPresetID(p.ID) {
			return nil, fmt.Errorf("%w: preset id %q must be 1..32 of [a-z0-9_-], starting with a letter or digit", ErrInvalidConfig, p.ID)
		}
		if !validAmount(p.AmountFen) {
			return nil, fmt.Errorf("%w: preset %q amount %d is outside 1..%d fen", ErrInvalidConfig, p.ID, p.AmountFen, MaxAmountFen)
		}
		if !validLabel(p.Label) {
			return nil, fmt.Errorf("%w: preset %q label must be 1..127 bytes of printable UTF-8 without surrounding spaces", ErrInvalidConfig, p.ID)
		}
		if _, dup := set.byID[p.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate preset id %q", ErrInvalidConfig, p.ID)
		}
		set.list = append(set.list, p)
		set.byID[p.ID] = p
	}
	return set, nil
}

func validPresetID(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case i > 0 && (r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func validLabel(s string) bool {
	if s == "" || len(s) > 127 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// Order is one recharge. AmountFen is what the provider bills and
// AmountMicroCNY is what the wallet is owed; the database keeps them equal.
type Order struct {
	ID              uuid.UUID   `json:"id"`
	MerchantOrderNo string      `json:"merchant_order_no"`
	UserID          uuid.UUID   `json:"user_id"`
	Provider        Provider    `json:"provider"`
	ProviderTradeNo string      `json:"provider_trade_no,omitempty"`
	AmountFen       Fen         `json:"amount_fen"`
	AmountMicroCNY  MicroCNY    `json:"amount_micro_cny"`
	Currency        string      `json:"currency"`
	Status          Status      `json:"status"`
	CreditState     CreditState `json:"credit_state"`
	Subject         string      `json:"subject"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	ExpiresAt       time.Time   `json:"expires_at"`
	PaidAt          *time.Time  `json:"paid_at,omitempty"`
	CreditedAt      *time.Time  `json:"credited_at,omitempty"`
}

// CurrencyCNY is the only currency this package bills in.
const CurrencyCNY = "CNY"

// clone returns a deep copy, so a store and its callers cannot mutate each
// other's order through shared pointers.
func (o *Order) clone() *Order {
	if o == nil {
		return nil
	}
	c := *o
	if o.PaidAt != nil {
		t := *o.PaidAt
		c.PaidAt = &t
	}
	if o.CreditedAt != nil {
		t := *o.CreditedAt
		c.CreditedAt = &t
	}
	return &c
}

// validate checks the fields every stored order needs.
func (o *Order) validate() error {
	if o == nil {
		return fmt.Errorf("%w: nil order", ErrInvalidOrder)
	}
	if o.ID == uuid.Nil {
		return fmt.Errorf("%w: order id is empty", ErrInvalidOrder)
	}
	if !validMerchantOrderNo(o.MerchantOrderNo) {
		return fmt.Errorf("%w: merchant order number %q", ErrInvalidOrder, o.MerchantOrderNo)
	}
	if o.UserID == uuid.Nil {
		return fmt.Errorf("%w: user id is empty", ErrInvalidOrder)
	}
	if !o.Provider.Valid() {
		return fmt.Errorf("%w: provider %q", ErrInvalidOrder, o.Provider)
	}
	if !validAmount(o.AmountFen) {
		return fmt.Errorf("%w: amount %d fen", ErrInvalidOrder, o.AmountFen)
	}
	if o.AmountMicroCNY != o.AmountFen.MicroCNY() {
		return fmt.Errorf("%w: amount %d micro-CNY does not match %d fen", ErrInvalidOrder, o.AmountMicroCNY, o.AmountFen)
	}
	if o.Currency != CurrencyCNY {
		return fmt.Errorf("%w: currency %q", ErrInvalidOrder, o.Currency)
	}
	if !validLabel(o.Subject) {
		return fmt.Errorf("%w: subject is not a usable label", ErrInvalidOrder)
	}
	if o.Status != StatusPending {
		return fmt.Errorf("%w: a new order must be pending, not %q", ErrInvalidOrder, o.Status)
	}
	if o.CreditState != CreditNone {
		return fmt.Errorf("%w: a new order owes no credit", ErrInvalidOrder)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() || o.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: order timestamps are missing", ErrInvalidOrder)
	}
	if !o.ExpiresAt.After(o.CreatedAt) {
		return fmt.Errorf("%w: expiry is not after creation", ErrInvalidOrder)
	}
	if o.PaidAt != nil || o.CreditedAt != nil {
		return fmt.Errorf("%w: a pending order has no payment or credit time", ErrInvalidOrder)
	}
	return nil
}

// markPaid moves a pending order to success with its credit still owed. The
// store must have locked the row.
func (o *Order) markPaid(tradeNo string, at time.Time) error {
	if o.Status != StatusPending {
		return fmt.Errorf("%w: order %s is %s", ErrOrderNotPending, o.MerchantOrderNo, o.Status)
	}
	if tradeNo == "" || len(tradeNo) > 64 {
		return fmt.Errorf("%w: provider trade number is empty or too long", ErrInvalidNotification)
	}
	if o.CreditState != CreditNone {
		return fmt.Errorf("%w: pending order %s already owes a credit", ErrInconsistentState, o.MerchantOrderNo)
	}
	o.Status = StatusSuccess
	o.ProviderTradeNo = tradeNo
	o.CreditState = CreditPending
	o.PaidAt = timePtr(at)
	o.UpdatedAt = at
	return nil
}

// markCredited records that the wallet ledger accepts the credit. It is only
// valid for a success order whose credit was owed.
func (o *Order) markCredited(at time.Time) error {
	if o.Status != StatusSuccess {
		return fmt.Errorf("%w: order %s is %s, not success", ErrInconsistentState, o.MerchantOrderNo, o.Status)
	}
	if o.CreditState != CreditPending {
		return fmt.Errorf("%w: order %s credit is %q, not pending", ErrInconsistentState, o.MerchantOrderNo, o.CreditState)
	}
	o.CreditState = CreditCredited
	o.CreditedAt = timePtr(at)
	o.UpdatedAt = at
	return nil
}

// markFailed moves a pending order to failed. A success order is never
// downgraded by a later notification.
func (o *Order) markFailed(at time.Time) error {
	if o.Status != StatusPending {
		return fmt.Errorf("%w: order %s is %s", ErrOrderNotPending, o.MerchantOrderNo, o.Status)
	}
	if o.CreditState != CreditNone {
		return fmt.Errorf("%w: order %s owes a credit", ErrInconsistentState, o.MerchantOrderNo)
	}
	o.Status = StatusFailed
	o.UpdatedAt = at
	return nil
}

// markExpired moves a pending order to expired.
func (o *Order) markExpired(at time.Time) error {
	if o.Status != StatusPending {
		return fmt.Errorf("%w: order %s is %s", ErrOrderNotPending, o.MerchantOrderNo, o.Status)
	}
	if o.CreditState != CreditNone {
		return fmt.Errorf("%w: order %s owes a credit", ErrInconsistentState, o.MerchantOrderNo)
	}
	o.Status = StatusExpired
	o.UpdatedAt = at
	return nil
}

// Notification is the non-secret part of one authenticated provider
// notification. It deliberately has no payer identity, raw body, signature or
// unverified text.
type Notification struct {
	Provider        Provider
	EventID         string
	EventType       string
	MerchantOrderNo string
	ProviderTradeNo string
	AmountFen       Fen
	Currency        string
	State           PaymentState
	PaidAt          time.Time
	PayloadSHA256   string
}

// EventOutcome is what a stored notification did to its order.
type EventOutcome string

const (
	// EventCreditPending means the payment was accepted and its wallet
	// credit is still owed.
	EventCreditPending EventOutcome = "credit_pending"

	// EventCredited means the wallet credit was recorded.
	EventCredited EventOutcome = "credited"

	// EventNotPaid means the notification did not report a completed
	// payment.
	EventNotPaid EventOutcome = "not_paid"

	// EventLate means the order was already failed, expired or otherwise
	// not creditable when the notification arrived.
	EventLate EventOutcome = "late"

	// EventDuplicate means the order was already credited and this
	// notification added nothing.
	EventDuplicate EventOutcome = "duplicate"
)

// Event is one stored, signature-verified provider notification.
type Event struct {
	ID                uuid.UUID
	Provider          Provider
	ProviderEventID   string
	OrderID           uuid.UUID
	MerchantOrderNo   string
	EventType         string
	ProviderTradeNo   string
	AmountFen         Fen
	SignatureVerified bool
	PayloadSHA256     string
	Outcome           EventOutcome
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// validMerchantOrderNo enforces the shape both providers accept and
// 003_payment.sql repeats: 6..32 of [0-9A-Za-z_|*-]. WeChat Pay's
// out_trade_no is the stricter of the two.
func validMerchantOrderNo(s string) bool {
	if len(s) < 6 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_' || r == '-' || r == '|' || r == '*':
		default:
			return false
		}
	}
	return true
}

// defaultMerchantOrderNo makes a unique-enough merchant order number that
// satisfies both providers: M + UTC time to the second + 48 random bits.
func defaultMerchantOrderNo(now time.Time) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("%w: merchant order number randomness: %v", ErrInvalidOrder, err)
	}
	return "M" + now.UTC().Format("20060102150405") + strings.ToUpper(hex.EncodeToString(b[:])), nil
}

func timePtr(t time.Time) *time.Time { return &t }

// chinaZone is UTC+8, the zone the official examples use for Alipay's
// timestamp and gmt_payment and for WeChat's time_expire.
var chinaZone = time.FixedZone("CST", 8*60*60)
