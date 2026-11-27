package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCreateOrderUsesPresetAmount(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	o := checkout.Order
	if o.AmountFen != 100 {
		t.Errorf("amount = %d fen, want the preset's 100", o.AmountFen)
	}
	if o.AmountMicroCNY != 1_000_000 {
		t.Errorf("micro-CNY = %d, want 1,000,000", o.AmountMicroCNY)
	}
	if o.Currency != CurrencyCNY || o.Status != StatusPending || o.CreditState != CreditNone {
		t.Errorf("order = %+v", o)
	}
	if o.Subject != "1元充值" {
		t.Errorf("subject = %q, want the preset label", o.Subject)
	}
	if !o.ExpiresAt.Equal(testNow.Add(30 * time.Minute)) {
		t.Errorf("expiry = %v", o.ExpiresAt)
	}
	if checkout.PayURL == "" {
		t.Error("no Alipay pay URL")
	}

	// The caller gets a copy of the presets, not the Service's own slice.
	presets := env.svc.Presets()
	presets[0].AmountFen = 1
	if got := env.svc.Presets()[0].AmountFen; got != 100 {
		t.Errorf("Presets() exposed its backing slice: first amount = %d", got)
	}

	if _, err := env.svc.CreateOrder(t.Context(), o.UserID, ProviderAlipay, "missing"); !errors.Is(err, ErrUnknownPreset) {
		t.Errorf("unknown preset error = %v", err)
	}
	if _, err := env.svc.CreateOrder(t.Context(), uuid.Nil, ProviderAlipay, "p1"); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("nil user error = %v", err)
	}
	if _, err := env.svc.CreateOrder(t.Context(), o.UserID, "paypal", "p1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("unknown provider error = %v", err)
	}
}

func TestCreateOrderRefusesDisabledProvider(t *testing.T) {
	env := newTestEnvWith(t, nil, func(c *Config) { c.WeChat = nil })
	if _, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderWeChat, "p1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("WeChat without a client = %v, want ErrProviderUnavailable", err)
	}
	if _, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderAlipay, "p1"); err != nil {
		t.Fatalf("Alipay while only WeChat is disabled: %v", err)
	}
}

func TestAlipayCallbackCreditsOnce(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	form := env.alipayPaidNotify(t, checkout.Order)

	result, err := env.svc.HandleAlipayNotify(t.Context(), form)
	if err != nil {
		t.Fatalf("HandleAlipayNotify: %v", err)
	}
	if !result.Ack || result.Reason != "credited" {
		t.Errorf("result = %+v", result)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("wallet balance = %d, want %d", got, checkout.Order.AmountMicroCNY)
	}
	if got := env.creditor.callCount(); got != 1 {
		t.Errorf("Credit calls = %d, want 1", got)
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusSuccess || o.CreditState != CreditCredited || o.ProviderTradeNo == "" || o.PaidAt == nil || o.CreditedAt == nil {
		t.Errorf("order after credit = %+v", o)
	}
	if got := env.store.countEvents(); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	e, ok := env.store.event(ProviderAlipay, "ALI-"+checkout.Order.MerchantOrderNo)
	if !ok || e.Outcome != EventCredited {
		t.Errorf("event = %+v, want credited", e)
	}

	// The provider retries the same notification: it is acknowledged and
	// adds nothing.
	result, err = env.svc.HandleAlipayNotify(t.Context(), form)
	if err != nil {
		t.Fatalf("duplicate HandleAlipayNotify: %v", err)
	}
	if !result.Ack || result.Reason != "already_credited" {
		t.Errorf("duplicate result = %+v", result)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance after duplicate = %d", got)
	}
	if got := env.creditor.callCount(); got != 1 {
		t.Errorf("Credit calls after duplicate = %d, want 1", got)
	}
	if got := env.store.countEvents(); got != 1 {
		t.Errorf("events after duplicate = %d, want 1", got)
	}
}

func TestAlipayCallbackRejectsTamperedAmount(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	// A valid signature over a smaller amount: the provider signature is
	// good, the amount is not the order's.
	form := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"out_trade_no": checkout.Order.MerchantOrderNo,
		"total_amount": "0.01",
		"notify_id":    "ALI-" + checkout.Order.MerchantOrderNo,
	})
	if _, err := env.svc.HandleAlipayNotify(t.Context(), form); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("tampered amount error = %v, want ErrAmountMismatch", err)
	}
	if got := env.creditor.balance(); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
	if got := env.creditor.callCount(); got != 0 {
		t.Errorf("Credit calls = %d, want 0", got)
	}
	if got := env.store.countEvents(); got != 0 {
		t.Errorf("events = %d, want 0", got)
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusPending || o.CreditState != CreditNone {
		t.Errorf("order after tampered callback = %+v", o)
	}
}

func TestAlipayCallbackAfterExpireIsLate(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	if err := env.svc.Expire(t.Context(), checkout.Order.MerchantOrderNo); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	result, err := env.svc.HandleAlipayNotify(t.Context(), env.alipayPaidNotify(t, checkout.Order))
	if err != nil {
		t.Fatalf("late callback: %v", err)
	}
	if !result.Ack || result.Reason != "late" {
		t.Errorf("late result = %+v", result)
	}
	if got := env.creditor.balance(); got != 0 {
		t.Errorf("late callback credited %d", got)
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusExpired {
		t.Errorf("order status = %q, want expired", o.Status)
	}
	e, ok := env.store.event(ProviderAlipay, "ALI-"+checkout.Order.MerchantOrderNo)
	if !ok || e.Outcome != EventLate {
		t.Errorf("late event = %+v", e)
	}
}

// A closed notification that arrives after a success must not downgrade the
// order or credit it twice.
func TestAlipayCallbackClosedAfterSuccessIsIgnored(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	if _, err := env.svc.HandleAlipayNotify(t.Context(), env.alipayPaidNotify(t, checkout.Order)); err != nil {
		t.Fatal(err)
	}
	balance := env.creditor.balance()
	closed := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"out_trade_no": checkout.Order.MerchantOrderNo,
		"total_amount": checkout.Order.AmountFen.YuanString(),
		"notify_id":    "ALI-CLOSED-" + checkout.Order.MerchantOrderNo,
		"trade_status": "TRADE_CLOSED",
	})
	result, err := env.svc.HandleAlipayNotify(t.Context(), closed)
	if err != nil {
		t.Fatalf("closed after success: %v", err)
	}
	if !result.Ack || result.Reason != "already_credited" {
		t.Errorf("closed after success result = %+v", result)
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusSuccess || o.CreditState != CreditCredited {
		t.Errorf("order after closed notification = %+v", o)
	}
	if got := env.creditor.balance(); got != balance {
		t.Errorf("balance changed from %d to %d", balance, got)
	}
	if got := env.store.countEvents(); got != 2 {
		t.Errorf("events = %d, want the success and the duplicate", got)
	}
}

// The wallet being down leaves the provider's payment recorded as success
// with the credit still owed; the same callback retried later credits once.
func TestCreditFailureLeavesPendingAndRetryCredits(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	env.creditor.failNext(1)
	form := env.alipayPaidNotify(t, checkout.Order)

	if _, err := env.svc.HandleAlipayNotify(t.Context(), form); err == nil {
		t.Fatal("callback succeeded while the wallet was down")
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusSuccess || o.CreditState != CreditPending {
		t.Fatalf("order after credit failure = %+v", o)
	}
	if got := env.creditor.balance(); got != 0 {
		t.Errorf("balance = %d after failed credit", got)
	}
	if got := env.creditor.callCount(); got != 1 {
		t.Errorf("Credit calls = %d, want 1", got)
	}
	e, ok := env.store.event(ProviderAlipay, "ALI-"+checkout.Order.MerchantOrderNo)
	if !ok || e.Outcome != EventCreditPending {
		t.Errorf("event after failed credit = %+v, want credit_pending", e)
	}

	result, err := env.svc.HandleAlipayNotify(t.Context(), form)
	if err != nil {
		t.Fatalf("retried callback: %v", err)
	}
	if !result.Ack || result.Reason != "credited" {
		t.Errorf("retried result = %+v", result)
	}
	o, err = env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.CreditState != CreditCredited {
		t.Errorf("order after retry = %+v", o)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d, want %d", got, checkout.Order.AmountMicroCNY)
	}
	if got := env.creditor.callCount(); got != 2 {
		t.Errorf("Credit calls = %d, want 2", got)
	}
	if got := env.store.countEvents(); got != 1 {
		t.Errorf("events = %d, want the same one", got)
	}
	e, ok = env.store.event(ProviderAlipay, "ALI-"+checkout.Order.MerchantOrderNo)
	if !ok || e.Outcome != EventCredited {
		t.Errorf("event after retry = %+v, want credited", e)
	}
}

func TestSettlePendingCredits(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	env.creditor.failNext(1)
	if _, err := env.svc.HandleAlipayNotify(t.Context(), env.alipayPaidNotify(t, checkout.Order)); err == nil {
		t.Fatal("callback succeeded while the wallet was down")
	}
	processed, err := env.svc.SettlePendingCredits(t.Context(), 10)
	if err != nil {
		t.Fatalf("SettlePendingCredits: %v", err)
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d", got)
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.CreditState != CreditCredited {
		t.Errorf("credit state = %q", o.CreditState)
	}
	// A second pass finds nothing.
	processed, err = env.svc.SettlePendingCredits(t.Context(), 10)
	if err != nil || processed != 0 {
		t.Errorf("second pass = %d, %v", processed, err)
	}
}

// With TransactionalCreditor a failed wallet transaction rolls the order's
// success and the payment event back too, so the provider can retry a
// genuinely pending order.
func TestTransactionalCreditorRollsBack(t *testing.T) {
	creditor := &txCreditor{fakeCreditor: newFakeCreditor(), fail: true}
	env := newTestEnvWith(t, creditor, nil)
	checkout := env.createOrder(t, ProviderAlipay)
	form := env.alipayPaidNotify(t, checkout.Order)

	if _, err := env.svc.HandleAlipayNotify(t.Context(), form); err == nil {
		t.Fatal("callback succeeded while CreditTx failed")
	}
	o, err := env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusPending || o.CreditState != CreditNone || o.ProviderTradeNo != "" {
		t.Fatalf("order after rolled back callback = %+v", o)
	}
	if got := env.store.countEvents(); got != 0 {
		t.Errorf("events = %d, want 0 after rollback", got)
	}
	if got := creditor.callCount(); got != 0 {
		t.Errorf("Credit calls = %d, want 0", got)
	}

	creditor.fail = false
	result, err := env.svc.HandleAlipayNotify(t.Context(), form)
	if err != nil {
		t.Fatalf("retried callback: %v", err)
	}
	if !result.Ack || result.Reason != "credited" {
		t.Errorf("retried result = %+v", result)
	}
	o, err = env.svc.Order(t.Context(), checkout.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusSuccess || o.CreditState != CreditCredited {
		t.Errorf("order after retry = %+v", o)
	}
	if got := creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d", got)
	}
	if got := creditor.callCount(); got != 1 {
		t.Errorf("Credit calls = %d, want 1", got)
	}
}

func TestTransactionalDuplicateCallbacksCreditOnce(t *testing.T) {
	creditor := &txCreditor{fakeCreditor: newFakeCreditor()}
	env := newTestEnvWith(t, creditor, nil)
	checkout := env.createOrder(t, ProviderAlipay)
	form := env.alipayPaidNotify(t, checkout.Order)

	results, errs := runCallbacks(t, 2, func() (NotifyResult, error) {
		return env.svc.HandleAlipayNotify(context.Background(), form)
	})
	for i := range errs {
		if errs[i] != nil {
			t.Errorf("callback %d: %v", i, errs[i])
		}
		if !results[i].Ack {
			t.Errorf("callback %d not acknowledged: %+v", i, results[i])
		}
	}
	if got := creditor.callCount(); got != 1 {
		t.Errorf("Credit calls = %d, want exactly 1", got)
	}
	if got := creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d, want one credit", got)
	}
	if got := env.store.countEvents(); got != 1 {
		t.Errorf("events = %d, want 1", got)
	}
}

// Without TransactionalCreditor a concurrent duplicate may call Credit
// again, but the same idempotency key makes the wallet record one entry.
func TestConcurrentDuplicateCallbacksDoNotDoubleCredit(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	form := env.alipayPaidNotify(t, checkout.Order)

	_, errs := runCallbacks(t, 2, func() (NotifyResult, error) {
		return env.svc.HandleAlipayNotify(context.Background(), form)
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("callback %d: %v", i, err)
		}
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d, want exactly one credit", got)
	}
	if got := env.creditor.callCount(); got < 1 || got > 2 {
		t.Errorf("Credit calls = %d, want 1 or 2", got)
	}
	if got := env.store.countEvents(); got != 1 {
		t.Errorf("events = %d, want one unique event", got)
	}
}

// One provider transaction may pay one order only.
func TestProviderTradeNoCannotPayTwoOrders(t *testing.T) {
	env := newTestEnv(t)
	first := env.createOrder(t, ProviderAlipay)
	second := env.createOrder(t, ProviderAlipay)
	sameTrade := "2026112622001400000000000009"

	firstNotify := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"out_trade_no": first.Order.MerchantOrderNo,
		"total_amount": first.Order.AmountFen.YuanString(),
		"notify_id":    "ALI-FIRST",
		"trade_no":     sameTrade,
	})
	if _, err := env.svc.HandleAlipayNotify(t.Context(), firstNotify); err != nil {
		t.Fatalf("first order: %v", err)
	}
	secondNotify := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"out_trade_no": second.Order.MerchantOrderNo,
		"total_amount": second.Order.AmountFen.YuanString(),
		"notify_id":    "ALI-SECOND",
		"trade_no":     sameTrade,
	})
	if _, err := env.svc.HandleAlipayNotify(t.Context(), secondNotify); !errors.Is(err, ErrTradeNoMismatch) {
		t.Fatalf("second order with the first order's trade number = %v, want ErrTradeNoMismatch", err)
	}
	if got := env.creditor.balance(); got != int64(first.Order.AmountMicroCNY) {
		t.Errorf("balance = %d, want only the first order's credit", got)
	}
	o, err := env.svc.Order(t.Context(), second.Order.MerchantOrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusPending || o.CreditState != CreditNone {
		t.Errorf("second order = %+v, want untouched", o)
	}
}

func TestWeChatCallbackCreditsOnce(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderWeChat)
	header, body := env.wechatPaidCallback(t, checkout.Order)

	result, err := env.svc.HandleWeChatNotify(t.Context(), header, body)
	if err != nil {
		t.Fatalf("HandleWeChatNotify: %v", err)
	}
	if !result.Ack || result.Reason != "credited" {
		t.Errorf("result = %+v", result)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance = %d", got)
	}
	result, err = env.svc.HandleWeChatNotify(t.Context(), header, body)
	if err != nil {
		t.Fatalf("duplicate HandleWeChatNotify: %v", err)
	}
	if !result.Ack || result.Reason != "already_credited" {
		t.Errorf("duplicate result = %+v", result)
	}
	if got := env.creditor.balance(); got != int64(checkout.Order.AmountMicroCNY) {
		t.Errorf("balance after duplicate = %d", got)
	}
	if got := env.creditor.callCount(); got != 1 {
		t.Errorf("Credit calls = %d", got)
	}
}

func TestWeChatCallbackTamperedAmountIsRefused(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderWeChat)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: checkout.Order.MerchantOrderNo,
		amountFen:  int64(checkout.Order.AmountFen) + 1,
	})
	if _, err := env.svc.HandleWeChatNotify(t.Context(), header, body); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("tampered amount error = %v, want ErrAmountMismatch", err)
	}
	if got := env.creditor.balance(); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}

func TestWeChatUnsupportedEventIsAcknowledged(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderWeChat)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: checkout.Order.MerchantOrderNo,
		amountFen:  int64(checkout.Order.AmountFen),
		eventID:    "EV-REFUND-1",
		eventType:  "REFUND.SUCCESS",
	})
	result, err := env.svc.HandleWeChatNotify(t.Context(), header, body)
	if err != nil {
		t.Fatalf("refund event: %v", err)
	}
	if !result.Ack || result.Reason != "unsupported_event" {
		t.Errorf("refund result = %+v", result)
	}
	if got := env.creditor.balance(); got != 0 {
		t.Errorf("refund event credited %d", got)
	}
}

func TestExpireRefusesSettledOrder(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	if _, err := env.svc.HandleAlipayNotify(t.Context(), env.alipayPaidNotify(t, checkout.Order)); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Expire(t.Context(), checkout.Order.MerchantOrderNo); !errors.Is(err, ErrOrderNotPending) {
		t.Fatalf("Expire on a success order = %v, want ErrOrderNotPending", err)
	}
}

func runCallbacks(t *testing.T, n int, fn func() (NotifyResult, error)) ([]NotifyResult, []error) {
	t.Helper()
	results := make([]NotifyResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = fn()
		}(i)
	}
	close(start)
	wg.Wait()
	return results, errs
}

func TestNewRefusesIncompleteConfig(t *testing.T) {
	valid := []Preset{{ID: "p1", AmountFen: 100, Label: "1元"}}
	store := newMemStore()
	base := Config{Store: store, Creditor: newFakeCreditor(), Presets: valid}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no store", Config{Creditor: base.Creditor, Presets: valid}},
		{"no creditor", Config{Store: store, Presets: valid}},
		{"no presets", Config{Store: store, Creditor: base.Creditor}},
		{"no provider", base},
		{"short ttl", func() Config { c := base; c.OrderTTL = time.Second; return c }()},
		{"long ttl", func() Config { c := base; c.OrderTTL = 25 * time.Hour; return c }()},
		{"zero alipay", func() Config { c := base; c.Alipay = &Alipay{}; return c }()},
		{"zero wechat", func() Config { c := base; c.WeChat = &WeChat{}; return c }()},
	}
	for _, tc := range cases {
		if _, err := New(tc.cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: New error = %v, want ErrInvalidConfig", tc.name, err)
		}
	}
}

func TestOrderNotFound(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.svc.Order(t.Context(), "MTEST999999"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("Order for a missing number = %v, want ErrOrderNotFound", err)
	}
}
