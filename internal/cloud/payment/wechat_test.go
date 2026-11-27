package payment

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWeChatCreateNativeSignsRequest(t *testing.T) {
	env := newTestEnv(t)
	checkout := env.createOrder(t, ProviderWeChat)
	if checkout.CodeURL != "weixin://wxpay/bizpayurl?pr=test123" {
		t.Fatalf("CodeURL = %q", checkout.CodeURL)
	}
	method, rawURL, headers, body := env.http.recorded(t)
	if method != http.MethodPost {
		t.Errorf("method = %q", method)
	}
	if rawURL != "https://api.mch.weixin.qq.com/v3/pay/transactions/native" {
		t.Errorf("URL = %q", rawURL)
	}
	auth := parseWeChatAuthorization(t, headers.Get("Authorization"))
	if auth["mchid"] != testWeChatMchID {
		t.Errorf("Authorization mchid = %q", auth["mchid"])
	}
	if auth["serial_no"] != testWeChatMerchantSN {
		t.Errorf("Authorization serial_no = %q", auth["serial_no"])
	}
	message := http.MethodPost + "\n" + wechatNativePath + "\n" + auth["timestamp"] + "\n" + auth["nonce_str"] + "\n" + string(body) + "\n"
	if err := verifySHA256RSA(&env.keys.wechatMerchant.PublicKey, message, auth["signature"]); err != nil {
		t.Fatalf("native request signature: %v", err)
	}

	var req struct {
		AppID       string `json:"appid"`
		MchID       string `json:"mchid"`
		Description string `json:"description"`
		OutTradeNo  string `json:"out_trade_no"`
		TimeExpire  string `json:"time_expire"`
		NotifyURL   string `json:"notify_url"`
		Amount      struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("native body: %v", err)
	}
	if req.AppID != testWeChatAppID || req.MchID != testWeChatMchID {
		t.Errorf("appid/mchid = %q/%q", req.AppID, req.MchID)
	}
	if req.OutTradeNo != checkout.Order.MerchantOrderNo {
		t.Errorf("out_trade_no = %q", req.OutTradeNo)
	}
	if req.Amount.Total != 100 || req.Amount.Currency != CurrencyCNY {
		t.Errorf("amount = %d %s", req.Amount.Total, req.Amount.Currency)
	}
	wantExpire := checkout.Order.ExpiresAt.In(chinaZone).Format(time.RFC3339)
	if req.TimeExpire != wantExpire {
		t.Errorf("time_expire = %q, want %q", req.TimeExpire, wantExpire)
	}
	if req.NotifyURL != "https://shop.example.test/wechat/notify" {
		t.Errorf("notify_url = %q", req.NotifyURL)
	}
}

func TestWeChatCreateNativeAPIError(t *testing.T) {
	env := newTestEnv(t)
	env.http.status = http.StatusBadRequest
	env.http.response = `{"code":"PARAM_ERROR","message":"amount is invalid"}`
	checkout, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderWeChat, "p1")
	if !errors.Is(err, ErrProviderAPI) {
		t.Fatalf("CreateOrder error = %v, want ErrProviderAPI", err)
	}
	if !strings.Contains(err.Error(), "PARAM_ERROR") {
		t.Errorf("error %q does not name the provider code", err)
	}
	if checkout.Order.MerchantOrderNo == "" {
		t.Fatal("failed native request lost the pending order")
	}
	if checkout.Order.Status != StatusPending {
		t.Errorf("order status after provider error = %q, want pending", checkout.Order.Status)
	}
}

func TestWeChatCallbackParsesVerifiedResource(t *testing.T) {
	env := newTestEnv(t)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: "MTEST000001",
		amountFen:  100,
	})
	n, err := env.wechat.ParseNotify(header, body)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if n.Provider != ProviderWeChat || n.State != PaymentSucceeded {
		t.Errorf("notification = %+v", n)
	}
	if n.MerchantOrderNo != "MTEST000001" || n.ProviderTradeNo != "4200000000202611260000000001" {
		t.Errorf("order/trade = %q/%q", n.MerchantOrderNo, n.ProviderTradeNo)
	}
	if n.AmountFen != 100 || n.Currency != CurrencyCNY {
		t.Errorf("amount = %d %s", n.AmountFen, n.Currency)
	}
	if n.EventID != "EV-20261126120000000001" {
		t.Errorf("event id = %q", n.EventID)
	}
	if !n.PaidAt.Equal(time.Date(2026, 11, 26, 3, 59, 30, 0, time.UTC)) {
		t.Errorf("paid at = %v", n.PaidAt)
	}
	if !validSHA256Hex(n.PayloadSHA256) {
		t.Errorf("payload digest = %q", n.PayloadSHA256)
	}
}

func TestWeChatCallbackRejectsTamperedBody(t *testing.T) {
	env := newTestEnv(t)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: "MTEST000001",
		amountFen:  100,
		tamperBody: true,
	})
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered body error = %v, want ErrInvalidSignature", err)
	}
}

// A correctly signed body with a broken GCM tag is refused by the tag even
// though the platform signature is valid.
func TestWeChatCallbackRejectsBrokenGCMTag(t *testing.T) {
	env := newTestEnv(t)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: "MTEST000001",
		amountFen:  100,
	})
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	resource := envelope["resource"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(resource["ciphertext"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	resource["ciphertext"] = base64.StdEncoding.EncodeToString(raw)
	body, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := header.Get("Wechatpay-Timestamp")
	nonce := header.Get("Wechatpay-Nonce")
	sig, err := signSHA256RSA(env.keys.wechatPlatform, timestamp+"\n"+nonce+"\n"+string(body)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	header.Set("Wechatpay-Signature", sig)
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("broken GCM tag error = %v, want ErrInvalidSignature", err)
	}
}

func TestWeChatCallbackRejectsWrongMerchant(t *testing.T) {
	env := newTestEnv(t)
	for _, tc := range []struct {
		name    string
		opts    wechatCallbackOptions
		wantErr error
	}{
		{"mchid", wechatCallbackOptions{outTradeNo: "MTEST000001", amountFen: 100, mchID: "1999999999"}, ErrMerchantMismatch},
		{"appid", wechatCallbackOptions{outTradeNo: "MTEST000001", amountFen: 100, appID: "wx9999999999999999"}, ErrMerchantMismatch},
		{"currency", wechatCallbackOptions{outTradeNo: "MTEST000001", amountFen: 100, currency: "USD"}, ErrCurrencyMismatch},
		{"state", wechatCallbackOptions{outTradeNo: "MTEST000001", amountFen: 100, tradeState: "NOTPAY"}, ErrInvalidNotification},
	} {
		header, body := wechatCallback(t, env.keys.wechatPlatform, tc.opts)
		if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestWeChatCallbackRejectsStaleTimestampAndUnknownSerial(t *testing.T) {
	env := newTestEnv(t)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: "MTEST000001",
		amountFen:  100,
		timestamp:  testNow.Add(-10 * time.Minute),
	})
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrStaleNotification) {
		t.Fatalf("stale timestamp error = %v, want ErrStaleNotification", err)
	}

	header, body = wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo:   "MTEST000001",
		amountFen:    100,
		headerSerial: "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF",
	})
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrUnknownPlatformCertificate) {
		t.Fatalf("unknown serial error = %v, want ErrUnknownPlatformCertificate", err)
	}
}

func TestWeChatCallbackUnsupportedEvent(t *testing.T) {
	env := newTestEnv(t)
	header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: "MTEST000001",
		amountFen:  100,
		eventID:    "EV-REFUND-1",
		eventType:  "REFUND.SUCCESS",
	})
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("refund event error = %v, want ErrUnsupportedEvent", err)
	}
}

func TestWeChatCallbackOversizedBody(t *testing.T) {
	env := newTestEnv(t)
	header, _ := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{outTradeNo: "MTEST000001", amountFen: 100})
	body := make([]byte, MaxNotificationBytes+1)
	if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrNotificationTooLarge) {
		t.Fatalf("oversized body error = %v, want ErrNotificationTooLarge", err)
	}
}

// A callback timestamp at the edge of int64 must not overflow the skew
// comparison and be accepted as fresh.
func TestWeChatCallbackTimestampOverflowIsStale(t *testing.T) {
	env := newTestEnv(t)
	for _, ts := range []int64{math.MaxInt64, math.MinInt64, 0} {
		header, body := wechatCallback(t, env.keys.wechatPlatform, wechatCallbackOptions{
			outTradeNo: "MTEST000001",
			amountFen:  100,
		})
		timestamp := strconv.FormatInt(ts, 10)
		header.Set("Wechatpay-Timestamp", timestamp)
		sig, err := signSHA256RSA(env.keys.wechatPlatform, timestamp+"\n"+header.Get("Wechatpay-Nonce")+"\n"+string(body)+"\n")
		if err != nil {
			t.Fatal(err)
		}
		header.Set("Wechatpay-Signature", sig)
		if _, err := env.wechat.ParseNotify(header, body); !errors.Is(err, ErrStaleNotification) {
			t.Errorf("timestamp %d error = %v, want ErrStaleNotification", ts, err)
		}
	}
}

func TestWeChatCreateNativeRefusesUnusableReplies(t *testing.T) {
	env := newTestEnv(t)
	env.http.response = `{}`
	if _, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderWeChat, "p1"); !errors.Is(err, ErrProviderAPI) {
		t.Fatalf("empty code_url error = %v, want ErrProviderAPI", err)
	}
	env.http.response = `not json`
	if _, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderWeChat, "p1"); !errors.Is(err, ErrProviderAPI) {
		t.Fatalf("non-JSON reply error = %v, want ErrProviderAPI", err)
	}
	env.http.err = errors.New("connection refused")
	if _, err := env.svc.CreateOrder(t.Context(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProviderWeChat, "p1"); !errors.Is(err, ErrProviderAPI) {
		t.Fatalf("transport error = %v, want ErrProviderAPI", err)
	}
}
