package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultOrderTTL is how long a recharge order stays payable when the
	// deployment does not name a TTL.
	DefaultOrderTTL = 30 * time.Minute

	minOrderTTL = time.Minute
	maxOrderTTL = 24 * time.Hour

	// defaultPendingCreditBatch bounds one SettlePendingCredits call.
	defaultPendingCreditBatch = 100
	maxPendingCreditBatch     = 1000
)

// Config is everything a Service needs. Store, Creditor and at least one
// provider are required; the other zero values have defaults.
type Config struct {
	// Store persists orders and payment events. Required.
	Store Store

	// Creditor adds a settled payment to the user's wallet. Required.
	Creditor Creditor

	// Presets are the server-controlled recharge amounts. Required.
	Presets []Preset

	// Alipay and WeChat are the configured provider clients. At least one
	// must be non-nil, and a nil one makes its provider unavailable.
	Alipay *Alipay
	WeChat *WeChat

	// OrderTTL is the payable lifetime of a new order. Zero means
	// DefaultOrderTTL.
	OrderTTL time.Duration

	// Now is the clock used for order timestamps and callback receive
	// times. Zero means time.Now. The Alipay and WeChat clients have their
	// own Now; a deployment sets all three to the same clock.
	Now func() time.Time

	// NewOrderID creates an order id. Zero means a random UUID.
	NewOrderID func() uuid.UUID

	// NewMerchantOrderNo creates the out_trade_no for a new order at the
	// given time. Zero means defaultMerchantOrderNo. The result must satisfy
	// both providers' 6..32 character [0-9A-Za-z_|*-] rule.
	NewMerchantOrderNo func(now time.Time) (string, error)
}

// Service creates recharge orders, verifies provider callbacks and credits
// the wallet through its Creditor. It is safe for concurrent use.
type Service struct {
	store      Store
	creditor   Creditor
	presets    *presetSet
	alipay     *Alipay
	wechat     *WeChat
	orderTTL   time.Duration
	now        func() time.Time
	newOrderID func() uuid.UUID
	newOrderNo func(time.Time) (string, error)
}

// Checkout is a created order and the provider's way to pay it. PayURL is set
// for Alipay page pay; CodeURL is set for WeChat Native. When the creation of
// the provider request fails, Order is still returned with its database row
// pending, and the error says why.
type Checkout struct {
	Order   Order  `json:"order"`
	PayURL  string `json:"pay_url,omitempty"`
	CodeURL string `json:"code_url,omitempty"`
}

// NotifyResult tells the caller how to answer the provider's callback. Only
// Ack == true may be answered with Alipay's "success" or WeChat's 200/204.
// Reason is a short, non-secret label for logs and tests.
type NotifyResult struct {
	Ack    bool   `json:"ack"`
	Reason string `json:"reason"`
}

// New validates cfg and returns a Service. It never contacts a provider or a
// database.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: Store is required", ErrInvalidConfig)
	}
	if cfg.Creditor == nil {
		return nil, fmt.Errorf("%w: Creditor is required", ErrInvalidConfig)
	}
	presets, err := newPresetSet(cfg.Presets)
	if err != nil {
		return nil, err
	}
	if cfg.Alipay == nil && cfg.WeChat == nil {
		return nil, fmt.Errorf("%w: at least one of Alipay or WeChat is required", ErrInvalidConfig)
	}
	if cfg.Alipay != nil && !cfg.Alipay.valid() {
		return nil, fmt.Errorf("%w: Alipay client is not initialized", ErrInvalidConfig)
	}
	if cfg.WeChat != nil && !cfg.WeChat.valid() {
		return nil, fmt.Errorf("%w: WeChat client is not initialized", ErrInvalidConfig)
	}
	ttl := cfg.OrderTTL
	if ttl == 0 {
		ttl = DefaultOrderTTL
	}
	if ttl < minOrderTTL || ttl > maxOrderTTL {
		return nil, fmt.Errorf("%w: OrderTTL %s is outside %s..%s", ErrInvalidConfig, ttl, minOrderTTL, maxOrderTTL)
	}
	s := &Service{
		store:      cfg.Store,
		creditor:   cfg.Creditor,
		presets:    presets,
		alipay:     cfg.Alipay,
		wechat:     cfg.WeChat,
		orderTTL:   ttl,
		now:        cfg.Now,
		newOrderID: cfg.NewOrderID,
		newOrderNo: cfg.NewMerchantOrderNo,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newOrderID == nil {
		s.newOrderID = func() uuid.UUID {
			id, err := uuid.NewRandom()
			if err != nil {
				return uuid.Nil
			}
			return id
		}
	}
	if s.newOrderNo == nil {
		s.newOrderNo = defaultMerchantOrderNo
	}
	return s, nil
}

// Presets returns the server-controlled amounts in configuration order. The
// returned slice is a copy; callers cannot change what CreateOrder accepts.
// AvailableProviders names only the providers configured on this deployment.
func (s *Service) AvailableProviders() []Provider {
	var out []Provider
	if s.alipay != nil {
		out = append(out, ProviderAlipay)
	}
	if s.wechat != nil {
		out = append(out, ProviderWeChat)
	}
	return out
}

// OrdersForUser lists one user's newest orders when the store is PostgreSQL.
func (s *Service) OrdersForUser(ctx context.Context, userID uuid.UUID, limit int) ([]Order, error) {
	store, ok := s.store.(interface {
		OrdersByUser(context.Context, uuid.UUID, int) ([]Order, error)
	})
	if !ok {
		return nil, fmt.Errorf("%w: user order history is unavailable", ErrInvalidConfig)
	}
	return store.OrdersByUser(ctx, userID, limit)
}

func (s *Service) Presets() []Preset {
	out := make([]Preset, len(s.presets.list))
	copy(out, s.presets.list)
	return out
}

// CreateOrder creates a pending order for one preset and asks the provider
// for a way to pay it. The amount is always the preset's; no caller input
// reaches it. A provider API failure leaves the order pending and returns it
// in Checkout together with the error.
func (s *Service) CreateOrder(ctx context.Context, userID uuid.UUID, provider Provider, presetID string) (Checkout, error) {
	if userID == uuid.Nil {
		return Checkout{}, fmt.Errorf("%w: user id is empty", ErrInvalidOrder)
	}
	preset, ok := s.presets.byID[presetID]
	if !ok {
		return Checkout{}, fmt.Errorf("%w: %q", ErrUnknownPreset, presetID)
	}
	if !provider.Valid() {
		return Checkout{}, fmt.Errorf("%w: provider %q", ErrProviderUnavailable, provider)
	}
	if (provider == ProviderAlipay && s.alipay == nil) || (provider == ProviderWeChat && s.wechat == nil) {
		return Checkout{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, provider)
	}
	now, err := s.currentTime()
	if err != nil {
		return Checkout{}, err
	}
	merchantNo, err := s.newOrderNo(now)
	if err != nil {
		return Checkout{}, err
	}
	if !validMerchantOrderNo(merchantNo) {
		return Checkout{}, fmt.Errorf("%w: generated merchant order number %q", ErrInvalidOrder, merchantNo)
	}
	id := s.newOrderID()
	if id == uuid.Nil {
		return Checkout{}, fmt.Errorf("%w: generated order id is empty", ErrInvalidOrder)
	}
	order := &Order{
		ID:              id,
		MerchantOrderNo: merchantNo,
		UserID:          userID,
		Provider:        provider,
		AmountFen:       preset.AmountFen,
		AmountMicroCNY:  preset.AmountFen.MicroCNY(),
		Currency:        CurrencyCNY,
		Status:          StatusPending,
		CreditState:     CreditNone,
		Subject:         preset.Label,
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now.Add(s.orderTTL),
	}
	if err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.InsertOrder(ctx, order)
	}); err != nil {
		return Checkout{}, err
	}
	checkout := Checkout{Order: *order.clone()}
	switch provider {
	case ProviderAlipay:
		payURL, err := s.alipay.PagePayURL(order)
		if err != nil {
			return checkout, err
		}
		checkout.PayURL = payURL
	case ProviderWeChat:
		codeURL, err := s.wechat.CreateNative(ctx, order)
		if err != nil {
			return checkout, err
		}
		checkout.CodeURL = codeURL
	}
	return checkout, nil
}

// Order reads one order without locking it.
func (s *Service) Order(ctx context.Context, merchantOrderNo string) (Order, error) {
	o, err := s.store.OrderByMerchantNo(ctx, merchantOrderNo)
	if err != nil {
		return Order{}, err
	}
	if o == nil {
		return Order{}, ErrOrderNotFound
	}
	return *o.clone(), nil
}

// Expire marks a pending order expired. The caller must have closed the
// provider's order first: this package does not call Alipay's or WeChat's
// close API. A payment that arrives after this call is a late callback and is
// acknowledged without crediting, so the deployment must reconcile it
// manually.
func (s *Service) Expire(ctx context.Context, merchantOrderNo string) error {
	if !validMerchantOrderNo(merchantOrderNo) {
		return fmt.Errorf("%w: merchant order number %q", ErrInvalidOrder, merchantOrderNo)
	}
	now, err := s.currentTime()
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		o, err := tx.LockOrderByMerchantNo(ctx, merchantOrderNo)
		if err != nil {
			return err
		}
		if o.Status == StatusExpired {
			return nil
		}
		if err := o.markExpired(now); err != nil {
			return err
		}
		return tx.SaveOrder(ctx, o)
	})
}

// HandleAlipayNotify verifies and settles one Alipay asynchronous
// notification. A nil error with Ack true means answer "success"; an error
// means answer "failure" so Alipay retries.
func (s *Service) HandleAlipayNotify(ctx context.Context, form url.Values) (NotifyResult, error) {
	if s.alipay == nil {
		return NotifyResult{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, ProviderAlipay)
	}
	n, err := s.alipay.ParseNotify(form)
	if err != nil {
		return NotifyResult{}, err
	}
	return s.settle(ctx, n)
}

// HandleWeChatNotify verifies and settles one WeChat Pay API v3 callback. A
// nil error with Ack true means answer HTTP 200 or 204; an error means answer
// a 4xx/5xx so WeChat retries. An authentic event this package does not
// settle (a refund notification) is acknowledged with Reason
// "unsupported_event".
func (s *Service) HandleWeChatNotify(ctx context.Context, header http.Header, body []byte) (NotifyResult, error) {
	if s.wechat == nil {
		return NotifyResult{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, ProviderWeChat)
	}
	n, err := s.wechat.ParseNotify(header, body)
	if errors.Is(err, ErrUnsupportedEvent) {
		return NotifyResult{Ack: true, Reason: "unsupported_event"}, nil
	}
	if err != nil {
		return NotifyResult{}, err
	}
	return s.settle(ctx, n)
}

// settle runs the whole callback decision for an already verified
// notification. Every read and write of the order happens under its row lock.
func (s *Service) settle(ctx context.Context, n *Notification) (NotifyResult, error) {
	if err := validateNotification(n); err != nil {
		return NotifyResult{}, err
	}
	now, err := s.currentTime()
	if err != nil {
		return NotifyResult{}, err
	}
	tc, transactional := s.creditor.(TransactionalCreditor)

	var (
		reason   string
		deferred *Order
	)
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		o, err := tx.LockOrderByMerchantNo(ctx, n.MerchantOrderNo)
		if err != nil {
			return err
		}
		if err := validateForOrder(o, n); err != nil {
			return err
		}
		switch o.Status {
		case StatusFailed, StatusExpired:
			// Out of order: a payment reported after the order stopped
			// being payable. Acknowledge so the provider stops retrying,
			// but never credit.
			if _, err := s.recordEvent(ctx, tx, o, n, EventLate, now); err != nil {
				return err
			}
			reason = "late"
			return nil
		case StatusSuccess:
			if o.CreditState == CreditCredited {
				if _, err := s.recordEvent(ctx, tx, o, n, EventDuplicate, now); err != nil {
					return err
				}
				reason = "already_credited"
				return nil
			}
			if o.CreditState != CreditPending {
				return fmt.Errorf("%w: success order %s has credit state %q", ErrInconsistentState, o.MerchantOrderNo, o.CreditState)
			}
			// The payment was accepted by an earlier callback; this one
			// only gets to retry the still-owed wallet credit.
			if _, err := s.recordEvent(ctx, tx, o, n, EventCreditPending, now); err != nil {
				return err
			}
			if transactional {
				if err := s.creditInTx(ctx, tx, tc, o, now); err != nil {
					return err
				}
				reason = "credited"
				return nil
			}
			deferred = o
			reason = "credited"
			return nil
		case StatusPending:
			switch n.State {
			case PaymentSucceeded:
				if err := o.markPaid(n.ProviderTradeNo, paidAt(n, now)); err != nil {
					return err
				}
				created, err := s.recordEvent(ctx, tx, o, n, EventCreditPending, now)
				if err != nil {
					return err
				}
				if !created {
					// The notification is already recorded while the
					// order is still pending: the rows disagree.
					return fmt.Errorf("%w: event %s exists while order %s is pending", ErrInconsistentState, n.EventID, o.MerchantOrderNo)
				}
				if err := tx.SaveOrder(ctx, o); err != nil {
					return err
				}
				if transactional {
					if err := s.creditInTx(ctx, tx, tc, o, now); err != nil {
						return err
					}
					reason = "credited"
					return nil
				}
				deferred = o
				reason = "credited"
				return nil
			case PaymentWaiting:
				if _, err := s.recordEvent(ctx, tx, o, n, EventNotPaid, now); err != nil {
					return err
				}
				reason = "not_paid"
				return nil
			case PaymentClosed, PaymentFailed:
				if err := o.markFailed(now); err != nil {
					return err
				}
				if err := tx.SaveOrder(ctx, o); err != nil {
					return err
				}
				if _, err := s.recordEvent(ctx, tx, o, n, EventNotPaid, now); err != nil {
					return err
				}
				reason = "not_paid"
				return nil
			default:
				return fmt.Errorf("%w: payment state %q", ErrInvalidNotification, n.State)
			}
		default:
			return fmt.Errorf("%w: order %s has status %q", ErrInconsistentState, o.MerchantOrderNo, o.Status)
		}
	})
	if err != nil {
		return NotifyResult{}, err
	}
	if deferred != nil {
		if err := s.creditOutsideTx(ctx, deferred, now); err != nil {
			return NotifyResult{}, err
		}
	}
	return NotifyResult{Ack: true, Reason: reason}, nil
}

// RetryCredit settles the wallet credit of a success order whose
// credit_state is still pending. It returns nil when no credit is owed. This
// is the recovery path for a callback whose Credit call failed after the
// order was marked success.
func (s *Service) RetryCredit(ctx context.Context, merchantOrderNo string) error {
	if !validMerchantOrderNo(merchantOrderNo) {
		return fmt.Errorf("%w: merchant order number %q", ErrInvalidOrder, merchantOrderNo)
	}
	now, err := s.currentTime()
	if err != nil {
		return err
	}
	tc, transactional := s.creditor.(TransactionalCreditor)
	var deferred *Order
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		o, err := tx.LockOrderByMerchantNo(ctx, merchantOrderNo)
		if err != nil {
			return err
		}
		if o.Status != StatusSuccess || o.CreditState == CreditCredited {
			return nil
		}
		if o.CreditState != CreditPending {
			return fmt.Errorf("%w: success order %s has credit state %q", ErrInconsistentState, o.MerchantOrderNo, o.CreditState)
		}
		if transactional {
			return s.creditInTx(ctx, tx, tc, o, now)
		}
		deferred = o
		return nil
	})
	if err != nil {
		return err
	}
	if deferred != nil {
		return s.creditOutsideTx(ctx, deferred, now)
	}
	return nil
}

// SettlePendingCredits replays up to limit success orders whose wallet credit
// is still pending, oldest first. It returns how many orders it processed and
// the first error; it keeps going after an error so one bad order does not
// block the rest. Callers run it periodically and after a Credit failure.
func (s *Service) SettlePendingCredits(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = defaultPendingCreditBatch
	}
	if limit > maxPendingCreditBatch {
		limit = maxPendingCreditBatch
	}
	orders, err := s.store.PendingCreditOrders(ctx, limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	var firstErr error
	for _, merchantOrderNo := range orders {
		if err := s.RetryCredit(ctx, merchantOrderNo); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		processed++
	}
	return processed, firstErr
}

// creditInTx is the atomic path: the wallet ledger write shares the
// transaction that holds the order's row lock.
func (s *Service) creditInTx(ctx context.Context, tx Tx, tc TransactionalCreditor, o *Order, now time.Time) error {
	if err := tc.CreditTx(ctx, tx, o.UserID, int64(o.AmountMicroCNY), creditKey(o), creditDescription(o)); err != nil {
		return fmt.Errorf("payment: credit order %s: %w", o.MerchantOrderNo, err)
	}
	if err := o.markCredited(now); err != nil {
		return err
	}
	if err := tx.SaveOrder(ctx, o); err != nil {
		return err
	}
	return tx.MarkCreditPendingEventsCredited(ctx, o.ID)
}

// creditOutsideTx calls a non-transactional Creditor after the order's
// success is committed, then records the credit. A failed Credit leaves the
// order success with credit_state pending, and the retry path (or the next
// duplicate callback) calls Credit again with the same idempotency key.
func (s *Service) creditOutsideTx(ctx context.Context, o *Order, now time.Time) error {
	if err := s.creditor.Credit(ctx, o.UserID, int64(o.AmountMicroCNY), creditKey(o), creditDescription(o)); err != nil {
		return fmt.Errorf("payment: credit order %s: %w", o.MerchantOrderNo, err)
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		cur, err := tx.LockOrderByMerchantNo(ctx, o.MerchantOrderNo)
		if err != nil {
			return err
		}
		switch cur.CreditState {
		case CreditCredited:
			// Another callback or retry recorded it first. The Credit call
			// above replayed the same key and added nothing. Mark any event
			// this callback inserted while the credit was still owed.
			return tx.MarkCreditPendingEventsCredited(ctx, cur.ID)
		case CreditPending:
			if cur.Status != StatusSuccess {
				return fmt.Errorf("%w: order %s is %s with credit pending", ErrInconsistentState, cur.MerchantOrderNo, cur.Status)
			}
			if err := cur.markCredited(now); err != nil {
				return err
			}
			if err := tx.SaveOrder(ctx, cur); err != nil {
				return err
			}
			return tx.MarkCreditPendingEventsCredited(ctx, cur.ID)
		default:
			return fmt.Errorf("%w: order %s has credit state %q", ErrInconsistentState, cur.MerchantOrderNo, cur.CreditState)
		}
	})
}

// recordEvent stores one verified notification. It returns false when the
// provider's event id was already recorded.
func (s *Service) recordEvent(ctx context.Context, tx Tx, o *Order, n *Notification, outcome EventOutcome, now time.Time) (bool, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return false, fmt.Errorf("%w: generate event id: %v", ErrInvalidOrder, err)
	}
	e := &Event{
		ID:                id,
		Provider:          n.Provider,
		ProviderEventID:   n.EventID,
		OrderID:           o.ID,
		MerchantOrderNo:   o.MerchantOrderNo,
		EventType:         n.EventType,
		ProviderTradeNo:   n.ProviderTradeNo,
		AmountFen:         n.AmountFen,
		SignatureVerified: true,
		PayloadSHA256:     n.PayloadSHA256,
		Outcome:           outcome,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	return tx.InsertEvent(ctx, e)
}

// validateNotification checks a verified notification's own fields before it
// reaches the database. The provider parsers check most of this already; the
// repeat is deliberate, because Store implementations can be called with a
// Notification built by code outside this package.
func validateNotification(n *Notification) error {
	if n == nil {
		return fmt.Errorf("%w: nil notification", ErrInvalidNotification)
	}
	if !n.Provider.Valid() {
		return fmt.Errorf("%w: provider %q", ErrInvalidNotification, n.Provider)
	}
	if n.EventID == "" || len(n.EventID) > 128 {
		return fmt.Errorf("%w: event id is empty or too long", ErrInvalidNotification)
	}
	if n.EventType == "" || len(n.EventType) > 64 {
		return fmt.Errorf("%w: event type is empty or too long", ErrInvalidNotification)
	}
	if !validMerchantOrderNo(n.MerchantOrderNo) {
		return fmt.Errorf("%w: merchant order number %q", ErrInvalidNotification, n.MerchantOrderNo)
	}
	if !validAmount(n.AmountFen) {
		return fmt.Errorf("%w: amount %d fen", ErrInvalidNotification, n.AmountFen)
	}
	if n.Currency != CurrencyCNY {
		return fmt.Errorf("%w: %q", ErrCurrencyMismatch, n.Currency)
	}
	if !validPaymentState(n.State) {
		return fmt.Errorf("%w: payment state %q", ErrInvalidNotification, n.State)
	}
	if n.State == PaymentSucceeded && (n.ProviderTradeNo == "" || len(n.ProviderTradeNo) > 64) {
		return fmt.Errorf("%w: provider trade number is empty or too long", ErrInvalidNotification)
	}
	if !validSHA256Hex(n.PayloadSHA256) {
		return fmt.Errorf("%w: payload digest", ErrInvalidNotification)
	}
	return nil
}

// validateForOrder checks the notification against the locked order. These
// are the checks that stop a valid signature for one merchant, currency or
// amount from crediting a different order.
func validateForOrder(o *Order, n *Notification) error {
	if o.Provider != n.Provider {
		return fmt.Errorf("%w: order %s is %s, notification is %s", ErrProviderMismatch, o.MerchantOrderNo, o.Provider, n.Provider)
	}
	if o.Currency != CurrencyCNY || n.Currency != CurrencyCNY {
		return fmt.Errorf("%w: order %q notification %q", ErrCurrencyMismatch, o.Currency, n.Currency)
	}
	if n.AmountFen != o.AmountFen {
		return fmt.Errorf("%w: order %s is %d fen, notification is %d fen", ErrAmountMismatch, o.MerchantOrderNo, o.AmountFen, n.AmountFen)
	}
	if o.ProviderTradeNo != "" && n.ProviderTradeNo != "" && o.ProviderTradeNo != n.ProviderTradeNo {
		return fmt.Errorf("%w: order %s has %s, notification has %s", ErrTradeNoMismatch, o.MerchantOrderNo, o.ProviderTradeNo, n.ProviderTradeNo)
	}
	return nil
}

func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func creditKey(o *Order) string { return "payment:" + o.MerchantOrderNo }

func creditDescription(o *Order) string { return "余额充值 " + o.MerchantOrderNo }

func paidAt(n *Notification, now time.Time) time.Time {
	if n.PaidAt.IsZero() {
		return now
	}
	return n.PaidAt.UTC()
}

func (s *Service) currentTime() (time.Time, error) {
	now := s.now().UTC()
	if now.IsZero() {
		return time.Time{}, fmt.Errorf("%w: clock returned the zero time", ErrInvalidConfig)
	}
	return now, nil
}

// trimForError keeps a provider's message useful in an error without echoing
// an unbounded or multi-line body into logs.
func trimForError(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
