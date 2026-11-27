package payment

// Alipay's computer website payment (电脑网站支付, alipay.trade.page.pay) and
// its asynchronous notification.
//
// The request is signed with the app's RSA2 private key over the sorted
// non-empty parameters excluding sign; the final URL carries sign_type=RSA2
// and the base64 signature. The asynchronous notification is verified with
// Alipay's public key over the same canonical form excluding sign and
// sign_type, as Alipay's SDKs do. Only then are app_id, seller_id, the
// merchant order number, the currency and total_amount read.

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultAlipayGateway = "https://openapi.alipay.com/gateway.do"

	// maxAlipayNotificationBytes bounds the decoded form. A real
	// notification is a few KB; the bound stops a large upload from
	// becoming a memory problem before the signature is checked.
	maxAlipayNotificationBytes = 1 << 20

	alipayMethodPagePay = "alipay.trade.page.pay"
	alipayProductCode   = "FAST_INSTANT_TRADE_PAY"
	alipaySignType      = "RSA2"
)

// AlipayConfig configures Alipay page pay. AppID, PrivateKeyPEM,
// AlipayPublicKeyPEM and NotifyURL are required.
type AlipayConfig struct {
	// AppID is the Alipay application id; notifications must name it.
	AppID string

	// PrivateKeyPEM is the app's RSA private key (PKCS#1 or PKCS#8, PEM or
	// bare base64). It signs requests and never leaves this process.
	PrivateKeyPEM string

	// AlipayPublicKeyPEM verifies asynchronous notifications. It is
	// Alipay's key, not the app's.
	AlipayPublicKeyPEM string

	// SellerID, when set, must equal the notification's seller_id. It is
	// the merchant PID and guards against a valid notification for another
	// merchant under the same app.
	SellerID string

	// GatewayURL defaults to https://openapi.alipay.com/gateway.do.
	GatewayURL string

	// NotifyURL is the public HTTPS endpoint Alipay posts asynchronous
	// notifications to.
	NotifyURL string

	// ReturnURL is where Alipay sends the user's browser after payment. It
	// is optional and is not trusted for settlement.
	ReturnURL string

	// Now is the clock; zero means time.Now.
	Now func() time.Time
}

// Alipay is a configured Alipay page-pay client. It is safe for concurrent
// use.
type Alipay struct {
	appID      string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	sellerID   string
	gateway    string
	notifyURL  string
	returnURL  string
	now        func() time.Time
}

// NewAlipay validates cfg and builds the client. A missing key, a key below
// 2048 bits, an encrypted key, or a non-HTTPS endpoint is refused.
func NewAlipay(cfg AlipayConfig) (*Alipay, error) {
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		return nil, fmt.Errorf("%w: Alipay AppID is required", ErrInvalidConfig)
	}
	key, err := parseRSAPrivateKeyPEM(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	pub, err := parseRSAPublicKeyPEM(cfg.AlipayPublicKeyPEM)
	if err != nil {
		return nil, err
	}
	gateway := strings.TrimSpace(cfg.GatewayURL)
	if gateway == "" {
		gateway = defaultAlipayGateway
	}
	if err := requireHTTPSURL(gateway, "Alipay gateway"); err != nil {
		return nil, err
	}
	notify := strings.TrimSpace(cfg.NotifyURL)
	if notify == "" {
		return nil, fmt.Errorf("%w: Alipay notify URL is required", ErrInvalidConfig)
	}
	if err := requireHTTPSURL(notify, "Alipay notify URL"); err != nil {
		return nil, err
	}
	returnURL := strings.TrimSpace(cfg.ReturnURL)
	if returnURL != "" {
		if err := requireHTTPSURL(returnURL, "Alipay return URL"); err != nil {
			return nil, err
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Alipay{
		appID:      appID,
		privateKey: key,
		publicKey:  pub,
		sellerID:   strings.TrimSpace(cfg.SellerID),
		gateway:    gateway,
		notifyURL:  notify,
		returnURL:  returnURL,
		now:        now,
	}, nil
}

func (a *Alipay) valid() bool {
	return a != nil && a.appID != "" && a.privateKey != nil && a.publicKey != nil && a.gateway != "" && a.notifyURL != "" && a.now != nil
}

// alipayPagePayBiz is the signed biz_content of a page-pay request. Field
// order is fixed by the struct so the JSON is stable.
type alipayPagePayBiz struct {
	OutTradeNo     string `json:"out_trade_no"`
	ProductCode    string `json:"product_code"`
	TotalAmount    string `json:"total_amount"`
	Subject        string `json:"subject"`
	TimeoutExpress string `json:"timeout_express"`
}

// PagePayURL builds the signed redirect URL for one pending order. The
// amount and subject come from the stored order, never from the caller. An
// order past its expiry or not pending is refused.
func (a *Alipay) PagePayURL(o *Order) (string, error) {
	if !a.valid() {
		return "", fmt.Errorf("%w: Alipay client is not initialized", ErrInvalidConfig)
	}
	if o == nil {
		return "", fmt.Errorf("%w: nil order", ErrInvalidOrder)
	}
	if o.Provider != ProviderAlipay {
		return "", fmt.Errorf("%w: order %s is %s", ErrProviderMismatch, o.MerchantOrderNo, o.Provider)
	}
	if o.Status != StatusPending {
		return "", fmt.Errorf("%w: order %s is %s", ErrOrderNotPending, o.MerchantOrderNo, o.Status)
	}
	if !validAmount(o.AmountFen) || o.Currency != CurrencyCNY {
		return "", fmt.Errorf("%w: order %s amount or currency", ErrInvalidOrder, o.MerchantOrderNo)
	}
	if !validMerchantOrderNo(o.MerchantOrderNo) || !validLabel(o.Subject) {
		return "", fmt.Errorf("%w: order %s fields", ErrInvalidOrder, o.MerchantOrderNo)
	}
	now := a.now().UTC()
	if now.IsZero() {
		return "", fmt.Errorf("%w: clock returned the zero time", ErrInvalidConfig)
	}
	if !o.ExpiresAt.After(now) {
		return "", fmt.Errorf("%w: order %s", ErrOrderExpired, o.MerchantOrderNo)
	}
	remaining := o.ExpiresAt.Sub(now)
	minutes := int((remaining + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	biz, err := json.Marshal(alipayPagePayBiz{
		OutTradeNo:     o.MerchantOrderNo,
		ProductCode:    alipayProductCode,
		TotalAmount:    o.AmountFen.YuanString(),
		Subject:        o.Subject,
		TimeoutExpress: fmt.Sprintf("%dm", minutes),
	})
	if err != nil {
		return "", fmt.Errorf("payment: alipay biz_content: %w", err)
	}
	params := url.Values{}
	params.Set("app_id", a.appID)
	params.Set("method", alipayMethodPagePay)
	params.Set("format", "JSON")
	params.Set("charset", "utf-8")
	params.Set("sign_type", alipaySignType)
	params.Set("timestamp", now.In(chinaZone).Format("2006-01-02 15:04:05"))
	params.Set("version", "1.0")
	params.Set("notify_url", a.notifyURL)
	if a.returnURL != "" {
		params.Set("return_url", a.returnURL)
	}
	params.Set("biz_content", string(biz))
	canonical, err := alipayCanonical(params, map[string]struct{}{"sign": {}})
	if err != nil {
		return "", err
	}
	signature, err := signSHA256RSA(a.privateKey, canonical)
	if err != nil {
		return "", err
	}
	params.Set("sign", signature)
	return a.gateway + "?" + params.Encode(), nil
}

// ParseNotify verifies one asynchronous notification and returns its
// non-secret fields. The signature is checked before any business field is
// read. A missing or wrong sign_type, app_id, seller_id, currency or amount
// is refused.
func (a *Alipay) ParseNotify(form url.Values) (*Notification, error) {
	if !a.valid() {
		return nil, fmt.Errorf("%w: Alipay client is not initialized", ErrInvalidConfig)
	}
	if len(form) == 0 {
		return nil, fmt.Errorf("%w: empty Alipay notification", ErrInvalidNotification)
	}
	if alipayFormSize(form) > maxAlipayNotificationBytes {
		return nil, fmt.Errorf("%w: Alipay notification form", ErrNotificationTooLarge)
	}
	signature, err := alipaySingleValue(form, "sign")
	if err != nil || signature == "" {
		return nil, fmt.Errorf("%w: Alipay signature is missing", ErrInvalidSignature)
	}
	signType, err := alipaySingleValue(form, "sign_type")
	if err != nil || signType != alipaySignType {
		return nil, fmt.Errorf("%w: Alipay sign_type is not RSA2", ErrInvalidSignature)
	}
	canonical, err := alipayCanonical(form, map[string]struct{}{"sign": {}, "sign_type": {}})
	if err != nil {
		return nil, err
	}
	if err := verifySHA256RSA(a.publicKey, canonical, signature); err != nil {
		return nil, err
	}

	appID, err := alipayRequired(form, "app_id")
	if err != nil {
		return nil, err
	}
	if appID != a.appID {
		return nil, fmt.Errorf("%w: Alipay app_id", ErrMerchantMismatch)
	}
	if a.sellerID != "" {
		sellerID, err := alipayRequired(form, "seller_id")
		if err != nil {
			return nil, err
		}
		if sellerID != a.sellerID {
			return nil, fmt.Errorf("%w: Alipay seller_id", ErrMerchantMismatch)
		}
	}
	if notifyType, ok, err := alipayOptional(form, "notify_type"); err != nil {
		return nil, err
	} else if ok && notifyType != "" {
		if len(notifyType) > 64 {
			return nil, fmt.Errorf("%w: Alipay notify_type is too long", ErrInvalidNotification)
		}
		if notifyType != "trade_status_sync" {
			return nil, fmt.Errorf("%w: Alipay notify_type %q", ErrInvalidNotification, notifyType)
		}
	}

	outTradeNo, err := alipayRequired(form, "out_trade_no")
	if err != nil {
		return nil, err
	}
	if !validMerchantOrderNo(outTradeNo) {
		return nil, fmt.Errorf("%w: Alipay out_trade_no", ErrInvalidNotification)
	}
	total, err := alipayRequired(form, "total_amount")
	if err != nil {
		return nil, err
	}
	amount, err := FenFromYuan(total)
	if err != nil {
		return nil, fmt.Errorf("%w: Alipay total_amount %q", ErrInvalidNotification, total)
	}
	currency := CurrencyCNY
	if c, ok, err := alipayOptional(form, "currency"); err != nil {
		return nil, err
	} else if ok && c != "" {
		if len(c) > 8 {
			return nil, fmt.Errorf("%w: Alipay currency is too long", ErrInvalidNotification)
		}
		if c != CurrencyCNY {
			return nil, fmt.Errorf("%w: Alipay currency %q", ErrCurrencyMismatch, c)
		}
		currency = c
	}
	tradeStatus, err := alipayRequired(form, "trade_status")
	if err != nil {
		return nil, err
	}
	if len(tradeStatus) > 64 {
		return nil, fmt.Errorf("%w: Alipay trade_status is too long", ErrInvalidNotification)
	}
	state, err := alipayPaymentState(tradeStatus)
	if err != nil {
		return nil, err
	}
	tradeNo, _, err := alipayOptional(form, "trade_no")
	if err != nil {
		return nil, err
	}
	if state == PaymentSucceeded && tradeNo == "" {
		return nil, fmt.Errorf("%w: Alipay success has no trade_no", ErrInvalidNotification)
	}
	if tradeNo != "" && len(tradeNo) > 64 {
		return nil, fmt.Errorf("%w: Alipay trade_no is too long", ErrInvalidNotification)
	}

	notifyID, _, err := alipayOptional(form, "notify_id")
	if err != nil {
		return nil, err
	}
	digest := sha256Hex([]byte(canonical))
	eventID := notifyID
	if eventID == "" {
		// Some deployments forward a notification without notify_id. A
		// digest of the signed content is still stable for a replay and
		// unique for a different notification.
		eventID = "digest:" + digest
	}
	if len(eventID) > 128 {
		return nil, fmt.Errorf("%w: Alipay event id is too long", ErrInvalidNotification)
	}

	var paidAt time.Time
	if state == PaymentSucceeded {
		if gmt, ok, err := alipayOptional(form, "gmt_payment"); err != nil {
			return nil, err
		} else if ok && gmt != "" {
			// A time that does not parse stays unknown; the caller records
			// its own receive time rather than inventing a provider time.
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", gmt, chinaZone); err == nil {
				paidAt = t
			}
		}
	}

	return &Notification{
		Provider:        ProviderAlipay,
		EventID:         eventID,
		EventType:       tradeStatus,
		MerchantOrderNo: outTradeNo,
		ProviderTradeNo: tradeNo,
		AmountFen:       amount,
		Currency:        currency,
		State:           state,
		PaidAt:          paidAt,
		PayloadSHA256:   digest,
	}, nil
}

func alipayPaymentState(status string) (PaymentState, error) {
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		return PaymentSucceeded, nil
	case "WAIT_BUYER_PAY":
		return PaymentWaiting, nil
	case "TRADE_CLOSED":
		return PaymentClosed, nil
	default:
		return "", fmt.Errorf("%w: Alipay trade_status %q", ErrInvalidNotification, status)
	}
}

// alipayCanonical builds the string Alipay signs: the non-empty parameters
// except the excluded ones, sorted by name, joined as k=v&k=v with no URL
// encoding. Duplicate names (in any case) are refused, so a parameter cannot
// mean two things.
func alipayCanonical(values url.Values, exclude map[string]struct{}) (string, error) {
	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for key, list := range values {
		if key == "" {
			return "", fmt.Errorf("%w: empty Alipay parameter name", ErrInvalidNotification)
		}
		lower := strings.ToLower(key)
		if _, dup := seen[lower]; dup {
			return "", fmt.Errorf("%w: duplicate Alipay parameter %.64q", ErrInvalidNotification, key)
		}
		seen[lower] = struct{}{}
		if _, skip := exclude[lower]; skip {
			continue
		}
		if len(list) != 1 {
			return "", fmt.Errorf("%w: Alipay parameter %.64q has %d values", ErrInvalidNotification, key, len(list))
		}
		if list[0] == "" {
			// Alipay excludes empty values from its signature.
			continue
		}
		pairs = append(pairs, pair{key: key, value: list[0]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(p.value)
	}
	return b.String(), nil
}

func alipayRequired(values url.Values, key string) (string, error) {
	v, ok, err := alipayOptional(values, key)
	if err != nil {
		return "", err
	}
	if !ok || v == "" {
		return "", fmt.Errorf("%w: Alipay parameter %q is missing", ErrInvalidNotification, key)
	}
	return v, nil
}

func alipayOptional(values url.Values, key string) (string, bool, error) {
	list, ok := values[key]
	if !ok {
		return "", false, nil
	}
	if len(list) != 1 {
		return "", true, fmt.Errorf("%w: Alipay parameter %q has %d values", ErrInvalidNotification, key, len(list))
	}
	return list[0], true, nil
}

func alipaySingleValue(values url.Values, key string) (string, error) {
	list, ok := values[key]
	if !ok || len(list) != 1 {
		return "", fmt.Errorf("%w: Alipay parameter %q", ErrInvalidNotification, key)
	}
	return list[0], nil
}

// requireHTTPSURL refuses a URL that cannot carry a public, encrypted
// endpoint. The signature is not sent to the notify URL, but a plaintext
// callback would expose payment data.
func requireHTTPSURL(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%w: %s is not an absolute URL", ErrInvalidConfig, field)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: %s must use HTTPS", ErrInvalidConfig, field)
	}
	return nil
}

// alipayFormSize bounds the work before canonicalization hashes the form.
func alipayFormSize(values url.Values) int {
	total := 0
	for key, list := range values {
		total += len(key)
		for _, v := range list {
			total += len(v)
		}
	}
	return total
}
