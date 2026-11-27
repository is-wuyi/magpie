package payment

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestAlipayPagePayURLIsSigned(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	if checkout.PayURL == "" {
		t.Fatal("Alipay checkout has no PayURL")
	}
	values := verifyAlipayURL(t, &env.keys.alipayApp.PublicKey, checkout.PayURL)

	want := map[string]string{
		"app_id":     testAlipayAppID,
		"method":     "alipay.trade.page.pay",
		"format":     "JSON",
		"charset":    "utf-8",
		"sign_type":  "RSA2",
		"version":    "1.0",
		"notify_url": "https://shop.example.test/alipay/notify",
		"return_url": "https://shop.example.test/alipay/return",
		"timestamp":  testNow.In(chinaZone).Format("2006-01-02 15:04:05"),
	}
	for k, v := range want {
		if got := values.Get(k); got != v {
			t.Errorf("page pay %s = %q, want %q", k, got, v)
		}
	}
	var biz struct {
		OutTradeNo     string `json:"out_trade_no"`
		ProductCode    string `json:"product_code"`
		TotalAmount    string `json:"total_amount"`
		Subject        string `json:"subject"`
		TimeoutExpress string `json:"timeout_express"`
	}
	if err := json.Unmarshal([]byte(values.Get("biz_content")), &biz); err != nil {
		t.Fatalf("biz_content: %v", err)
	}
	if biz.OutTradeNo != checkout.Order.MerchantOrderNo {
		t.Errorf("biz out_trade_no = %q, want %q", biz.OutTradeNo, checkout.Order.MerchantOrderNo)
	}
	if biz.TotalAmount != "1.00" {
		t.Errorf("biz total_amount = %q, want 1.00", biz.TotalAmount)
	}
	if biz.ProductCode != alipayProductCode {
		t.Errorf("biz product_code = %q", biz.ProductCode)
	}
	if biz.Subject != checkout.Order.Subject {
		t.Errorf("biz subject = %q", biz.Subject)
	}
	if biz.TimeoutExpress != "30m" {
		t.Errorf("biz timeout_express = %q, want 30m", biz.TimeoutExpress)
	}
}

// A page-pay URL whose signed amount is changed no longer verifies with the
// app's key.
func TestAlipayPagePayURLTamperedAmountFailsVerification(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)
	u, err := url.Parse(checkout.PayURL)
	if err != nil {
		t.Fatal(err)
	}
	values := u.Query()
	var biz map[string]any
	if err := json.Unmarshal([]byte(values.Get("biz_content")), &biz); err != nil {
		t.Fatal(err)
	}
	biz["total_amount"] = "0.01"
	tampered, err := json.Marshal(biz)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("biz_content", string(tampered))
	signature := values.Get("sign")
	values.Del("sign")
	if err := verifySHA256RSA(&env.keys.alipayApp.PublicKey, independentCanonical(values), signature); err == nil {
		t.Fatal("a page-pay signature still verifies after the amount was changed")
	}
}

func TestAlipayPagePayURLRefusesUnpayableOrders(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderAlipay)

	expired := checkout.Order
	expired.ExpiresAt = testNow.Add(-time.Second)
	if _, err := env.alipay.PagePayURL(&expired); !errors.Is(err, ErrOrderExpired) {
		t.Fatalf("expired order error = %v, want ErrOrderExpired", err)
	}
	paid := checkout.Order
	paid.Status = StatusSuccess
	if _, err := env.alipay.PagePayURL(&paid); !errors.Is(err, ErrOrderNotPending) {
		t.Fatalf("success order error = %v, want ErrOrderNotPending", err)
	}
}

func TestAlipayParseNotify(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"out_trade_no": "MTEST000001",
		"total_amount": "1.00",
		"notify_id":    "ALI-EVENT-1",
	})
	n, err := env.alipay.ParseNotify(form)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if n.Provider != ProviderAlipay || n.State != PaymentSucceeded {
		t.Errorf("notification = %+v", n)
	}
	if n.MerchantOrderNo != "MTEST000001" || n.ProviderTradeNo != "2026112622001400000000000001" {
		t.Errorf("order/trade = %q/%q", n.MerchantOrderNo, n.ProviderTradeNo)
	}
	if n.AmountFen != 100 || n.Currency != CurrencyCNY {
		t.Errorf("amount = %d %s", n.AmountFen, n.Currency)
	}
	if n.EventID != "ALI-EVENT-1" {
		t.Errorf("event id = %q", n.EventID)
	}
	if !n.PaidAt.Equal(time.Date(2026, 11, 26, 12, 0, 0, 0, chinaZone)) {
		t.Errorf("paid at = %v", n.PaidAt)
	}
	if !validSHA256Hex(n.PayloadSHA256) {
		t.Errorf("payload digest = %q", n.PayloadSHA256)
	}
}

func TestAlipayParseNotifyRejectsBadSignature(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, nil)
	form.Set("total_amount", "0.01") // after signing
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered notification error = %v, want ErrInvalidSignature", err)
	}
}

func TestAlipayParseNotifyRejectsWrongMerchant(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"app_id": "2026999999999999"})
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrMerchantMismatch) {
		t.Fatalf("wrong app_id error = %v, want ErrMerchantMismatch", err)
	}

	form = alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"seller_id": "2088999999999999"})
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrMerchantMismatch) {
		t.Fatalf("wrong seller_id error = %v, want ErrMerchantMismatch", err)
	}
}

func TestAlipayParseNotifyRejectsMissingOrWrongSignType(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, nil)
	form.Del("sign_type")
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("missing sign_type error = %v, want ErrInvalidSignature", err)
	}

	form = alipayNotifyForm(t, env.keys.alipayProvider, nil)
	form.Set("sign_type", "RSA")
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("RSA sign_type error = %v, want ErrInvalidSignature", err)
	}
}

func TestAlipayParseNotifyStates(t *testing.T) {
	env := newTestEnv(t)

	closed := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{
		"trade_status": "TRADE_CLOSED",
		"trade_no":     "",
	})
	n, err := env.alipay.ParseNotify(closed)
	if err != nil {
		t.Fatalf("TRADE_CLOSED: %v", err)
	}
	if n.State != PaymentClosed {
		t.Errorf("TRADE_CLOSED state = %q", n.State)
	}

	unknown := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"trade_status": "TRADE_MAGIC"})
	if _, err := env.alipay.ParseNotify(unknown); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("unknown status error = %v, want ErrInvalidNotification", err)
	}

	successWithoutTradeNo := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"trade_no": ""})
	if _, err := env.alipay.ParseNotify(successWithoutTradeNo); !errors.Is(err, ErrInvalidNotification) {
		t.Fatalf("success without trade_no error = %v, want ErrInvalidNotification", err)
	}
}

func TestAlipayNotifyWithoutNotifyIDUsesSignedDigest(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"notify_id": ""})
	n, err := env.alipay.ParseNotify(form)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if n.EventID != "digest:"+n.PayloadSHA256 {
		t.Errorf("event id = %q, want digest of the signed content", n.EventID)
	}
}

func TestAlipayParseNotifyRejectsNonCNY(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, map[string]string{"currency": "USD"})
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("currency error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestAlipayParseNotifyRejectsDuplicateSignatureParameter(t *testing.T) {
	env := newTestEnv(t)
	form := alipayNotifyForm(t, env.keys.alipayProvider, nil)
	form["sign"] = []string{form.Get("sign"), form.Get("sign")}
	if _, err := env.alipay.ParseNotify(form); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("duplicate sign error = %v, want ErrInvalidSignature", err)
	}
}
