package payment

// WeChat Pay API v3 Native payment and its callback.
//
// A Native order is POSTed to /v3/pay/transactions/native with the merchant's
// WECHATPAY2-SHA256-RSA2048 Authorization. The callback is authenticated with
// the platform certificate named by Wechatpay-Serial over
// timestamp\nnonce\nbody\n, then its AES-256-GCM resource is decrypted with
// the API v3 key. Only after both checks are the appid, mchid, currency and
// amount trusted.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWeChatGateway = "https://api.mch.weixin.qq.com"

	wechatNativePath = "/v3/pay/transactions/native"
	wechatAuthScheme = "WECHATPAY2-SHA256-RSA2048"

	// DefaultWeChatCallbackSkew is how far a callback's
	// Wechatpay-Timestamp may be from this server's clock.
	DefaultWeChatCallbackSkew = 5 * time.Minute

	maxWeChatCallbackSkew = time.Hour

	// MaxNotificationBytes bounds a callback body. A WeChat transaction
	// callback is a few KB; the bound stops a large upload from becoming a
	// memory problem before the signature is checked.
	MaxNotificationBytes = 1 << 20

	maxProviderResponseBytes = 1 << 20
)

// HTTPClient is the HTTP seam for WeChat's API. Tests supply a fake; the
// real deployment can supply its own instrumented client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// WeChatConfig configures WeChat Pay API v3 Native. AppID, MchID,
// MerchantSerialNo, PrivateKeyPEM, APIv3Key, NotifyURL and at least one
// platform certificate are required.
type WeChatConfig struct {
	// AppID and MchID are the merchant identity; callbacks must name both.
	AppID string
	MchID string

	// MerchantSerialNo is the serial number of the merchant API
	// certificate whose private key signs requests.
	MerchantSerialNo string

	// PrivateKeyPEM is the merchant API certificate's RSA private key.
	PrivateKeyPEM string

	// APIv3Key decrypts callback resources. It is exactly 32 bytes and is
	// never sent anywhere.
	APIv3Key string

	// NotifyURL is the public HTTPS callback endpoint.
	NotifyURL string

	// PlatformCertificates are the PEM X.509 leaf certificates WeChat uses
	// to sign callbacks (the first CERTIFICATE block of each string). Supply
	// the current certificate and the next one during rotation; this package
	// does not download them.
	PlatformCertificates []string

	// GatewayURL defaults to https://api.mch.weixin.qq.com.
	GatewayURL string

	// HTTPClient defaults to a 15-second client that refuses redirects, so
	// an Authorization header is never replayed. A supplied client should
	// refuse redirects for the same reason.
	HTTPClient HTTPClient

	// Now is the clock; zero means time.Now.
	Now func() time.Time

	// CallbackSkew bounds how old a callback timestamp may be. Zero means
	// DefaultWeChatCallbackSkew.
	CallbackSkew time.Duration
}

// WeChat is a configured WeChat Pay API v3 client. It is safe for concurrent
// use.
type WeChat struct {
	appID      string
	mchID      string
	serialNo   string
	privateKey *rsa.PrivateKey
	apiV3Key   []byte
	notifyURL  string
	gateway    string
	platform   map[string]*x509.Certificate
	client     HTTPClient
	now        func() time.Time
	skew       time.Duration
}

// NewWeChat validates cfg and builds the client. A missing or short key, a
// non-HTTPS endpoint, an empty platform certificate set or a duplicate
// certificate serial is refused.
func NewWeChat(cfg WeChatConfig) (*WeChat, error) {
	appID := strings.TrimSpace(cfg.AppID)
	mchID := strings.TrimSpace(cfg.MchID)
	serial := strings.TrimSpace(cfg.MerchantSerialNo)
	if appID == "" || mchID == "" || serial == "" {
		return nil, fmt.Errorf("%w: WeChat AppID, MchID and merchant serial are required", ErrInvalidConfig)
	}
	key, err := parseRSAPrivateKeyPEM(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	if len(cfg.APIv3Key) != 32 {
		return nil, fmt.Errorf("%w: WeChat API v3 key must be exactly 32 bytes", ErrInvalidConfig)
	}
	if len(cfg.PlatformCertificates) == 0 {
		return nil, fmt.Errorf("%w: at least one WeChat platform certificate is required", ErrInvalidConfig)
	}
	platform := make(map[string]*x509.Certificate, len(cfg.PlatformCertificates))
	for _, text := range cfg.PlatformCertificates {
		cert, err := parseWeChatPlatformCertificate(text)
		if err != nil {
			return nil, err
		}
		certSerial := normalizeCertSerial(cert.SerialNumber.Text(16))
		if certSerial == "" {
			return nil, fmt.Errorf("%w: WeChat platform certificate has no serial", ErrInvalidConfig)
		}
		if _, dup := platform[certSerial]; dup {
			return nil, fmt.Errorf("%w: duplicate WeChat platform certificate serial", ErrInvalidConfig)
		}
		platform[certSerial] = cert
	}
	gateway := strings.TrimSpace(cfg.GatewayURL)
	if gateway == "" {
		gateway = defaultWeChatGateway
	}
	gateway = strings.TrimRight(gateway, "/")
	if err := requireHTTPSURL(gateway, "WeChat gateway"); err != nil {
		return nil, err
	}
	notify := strings.TrimSpace(cfg.NotifyURL)
	if notify == "" {
		return nil, fmt.Errorf("%w: WeChat notify URL is required", ErrInvalidConfig)
	}
	if err := requireHTTPSURL(notify, "WeChat notify URL"); err != nil {
		return nil, err
	}
	skew := cfg.CallbackSkew
	if skew == 0 {
		skew = DefaultWeChatCallbackSkew
	}
	if skew < 0 || skew > maxWeChatCallbackSkew {
		return nil, fmt.Errorf("%w: WeChat callback skew %s is outside 0..%s", ErrInvalidConfig, skew, maxWeChatCallbackSkew)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &WeChat{
		appID:      appID,
		mchID:      mchID,
		serialNo:   serial,
		privateKey: key,
		apiV3Key:   []byte(cfg.APIv3Key),
		notifyURL:  notify,
		gateway:    gateway,
		platform:   platform,
		client:     client,
		now:        now,
		skew:       skew,
	}, nil
}

func (w *WeChat) valid() bool {
	return w != nil && w.appID != "" && w.mchID != "" && w.serialNo != "" &&
		w.privateKey != nil && len(w.apiV3Key) == 32 && w.notifyURL != "" &&
		w.gateway != "" && len(w.platform) > 0 && w.client != nil && w.now != nil && w.skew >= 0
}

// wechatNativeRequest is the Native order body. It has no payer identity and
// no client-supplied amount: total is the stored order's fen.
type wechatNativeRequest struct {
	AppID       string       `json:"appid"`
	MchID       string       `json:"mchid"`
	Description string       `json:"description"`
	OutTradeNo  string       `json:"out_trade_no"`
	TimeExpire  string       `json:"time_expire"`
	NotifyURL   string       `json:"notify_url"`
	Amount      wechatAmount `json:"amount"`
}

type wechatAmount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

// CreateNative asks WeChat for a Native order and returns its code_url. The
// order must be pending and not past its expiry. A non-2xx reply or a body
// without code_url is ErrProviderAPI.
func (w *WeChat) CreateNative(ctx context.Context, o *Order) (string, error) {
	if !w.valid() {
		return "", fmt.Errorf("%w: WeChat client is not initialized", ErrInvalidConfig)
	}
	if o == nil {
		return "", fmt.Errorf("%w: nil order", ErrInvalidOrder)
	}
	if o.Provider != ProviderWeChat {
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
	now := w.now().UTC()
	if now.IsZero() {
		return "", fmt.Errorf("%w: clock returned the zero time", ErrInvalidConfig)
	}
	if !o.ExpiresAt.After(now) {
		return "", fmt.Errorf("%w: order %s", ErrOrderExpired, o.MerchantOrderNo)
	}
	body, err := json.Marshal(wechatNativeRequest{
		AppID:       w.appID,
		MchID:       w.mchID,
		Description: o.Subject,
		OutTradeNo:  o.MerchantOrderNo,
		TimeExpire:  o.ExpiresAt.In(chinaZone).Format(time.RFC3339),
		NotifyURL:   w.notifyURL,
		Amount:      wechatAmount{Total: int64(o.AmountFen), Currency: CurrencyCNY},
	})
	if err != nil {
		return "", fmt.Errorf("payment: wechat native body: %w", err)
	}
	nonce, err := wechatNonce()
	if err != nil {
		return "", err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature, err := wechatSignRequest(w.privateKey, http.MethodPost, wechatNativePath, timestamp, nonce, body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.gateway+wechatNativePath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("payment: wechat native request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf(
		`%s mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`,
		wechatAuthScheme, w.mchID, nonce, signature, timestamp, w.serialNo))
	resp, err := w.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return "", fmt.Errorf("%w: wechat native request: %v", ErrProviderAPI, err)
	}
	if resp == nil || resp.Body == nil {
		return "", fmt.Errorf("%w: wechat native response has no body", ErrProviderAPI)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: wechat native response: %v", ErrProviderAPI, err)
	}
	if len(reply) > maxProviderResponseBytes {
		return "", fmt.Errorf("%w: wechat native response is too large", ErrProviderAPI)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: wechat native status %d: %s", ErrProviderAPI, resp.StatusCode, wechatAPIError(reply))
	}
	var out struct {
		CodeURL string `json:"code_url"`
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		return "", fmt.Errorf("%w: wechat native response is not JSON", ErrProviderAPI)
	}
	if out.CodeURL == "" {
		return "", fmt.Errorf("%w: wechat native response has no code_url", ErrProviderAPI)
	}
	if len(out.CodeURL) > 4096 {
		return "", fmt.Errorf("%w: wechat code_url is too long", ErrProviderAPI)
	}
	return out.CodeURL, nil
}

// wechatCallbackEnvelope is the outer callback body. The resource is
// encrypted; nothing inside it is read before the signature verifies and GCM
// authenticates it.
type wechatCallbackEnvelope struct {
	ID           string `json:"id"`
	CreateTime   string `json:"create_time"`
	ResourceType string `json:"resource_type"`
	EventType    string `json:"event_type"`
	Summary      string `json:"summary"`
	Resource     struct {
		OriginalType   string `json:"original_type"`
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
		Nonce          string `json:"nonce"`
	} `json:"resource"`
}

// wechatTransaction is the decrypted transaction resource. Payer identity
// and promotion details are deliberately not decoded: the recharge only
// needs the order, the trade number, the state and the amount.
type wechatTransaction struct {
	AppID          string `json:"appid"`
	MchID          string `json:"mchid"`
	OutTradeNo     string `json:"out_trade_no"`
	TransactionID  string `json:"transaction_id"`
	TradeState     string `json:"trade_state"`
	TradeStateDesc string `json:"trade_state_desc"`
	SuccessTime    string `json:"success_time"`
	Amount         struct {
		Total         int64  `json:"total"`
		PayerTotal    int64  `json:"payer_total"`
		Currency      string `json:"currency"`
		PayerCurrency string `json:"payer_currency"`
	} `json:"amount"`
}

// ParseNotify verifies and decrypts one WeChat Pay callback. The signature
// and the GCM tag are both checked before any order, merchant or amount is
// read. An authentic event other than TRANSACTION.SUCCESS is
// ErrUnsupportedEvent; the caller acknowledges it without settling.
func (w *WeChat) ParseNotify(header http.Header, body []byte) (*Notification, error) {
	if !w.valid() {
		return nil, fmt.Errorf("%w: WeChat client is not initialized", ErrInvalidConfig)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty WeChat callback", ErrInvalidNotification)
	}
	if len(body) > MaxNotificationBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrNotificationTooLarge, len(body))
	}
	timestamp := strings.TrimSpace(header.Get("Wechatpay-Timestamp"))
	nonce := strings.TrimSpace(header.Get("Wechatpay-Nonce"))
	signature := strings.TrimSpace(header.Get("Wechatpay-Signature"))
	serial := normalizeCertSerial(header.Get("Wechatpay-Serial"))
	if timestamp == "" || nonce == "" || signature == "" || serial == "" {
		return nil, fmt.Errorf("%w: WeChat callback headers are incomplete", ErrInvalidSignature)
	}
	if sigType := strings.TrimSpace(header.Get("Wechatpay-Signature-Type")); sigType != "" && sigType != wechatAuthScheme {
		return nil, fmt.Errorf("%w: WeChat signature type is not %s", ErrInvalidSignature, wechatAuthScheme)
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: WeChat callback timestamp", ErrInvalidSignature)
	}
	now := w.now().UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("%w: clock returned the zero time", ErrInvalidConfig)
	}
	maxSkew := int64(w.skew / time.Second)
	nowSec := now.Unix()
	if ts < nowSec-maxSkew || ts > nowSec+maxSkew {
		return nil, fmt.Errorf("%w: WeChat callback timestamp is outside %s", ErrStaleNotification, w.skew)
	}
	if len(serial) > 128 {
		return nil, fmt.Errorf("%w: WeChat platform serial is too long", ErrUnknownPlatformCertificate)
	}
	cert, ok := w.platform[serial]
	if !ok {
		return nil, fmt.Errorf("%w: WeChat platform serial is unknown", ErrUnknownPlatformCertificate)
	}
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, fmt.Errorf("%w: certificate is outside its validity window", ErrUnknownPlatformCertificate)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: platform certificate is not RSA", ErrUnknownPlatformCertificate)
	}
	message := timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	if err := verifySHA256RSA(pub, message, signature); err != nil {
		return nil, err
	}

	var envelope wechatCallbackEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: WeChat callback is not JSON", ErrInvalidNotification)
	}
	if envelope.ID == "" || len(envelope.ID) > 128 {
		return nil, fmt.Errorf("%w: WeChat event id", ErrInvalidNotification)
	}
	if envelope.EventType == "" || len(envelope.EventType) > 64 {
		return nil, fmt.Errorf("%w: WeChat event type", ErrInvalidNotification)
	}
	if envelope.EventType != "TRANSACTION.SUCCESS" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEvent, envelope.EventType)
	}
	if envelope.ResourceType != "encrypt-resource" {
		return nil, fmt.Errorf("%w: WeChat resource_type is not encrypt-resource", ErrInvalidNotification)
	}
	if envelope.Resource.OriginalType != "transaction" {
		return nil, fmt.Errorf("%w: WeChat original_type is not transaction", ErrInvalidNotification)
	}
	if envelope.Resource.Algorithm != "AEAD_AES_256_GCM" {
		return nil, fmt.Errorf("%w: WeChat resource algorithm is not AEAD_AES_256_GCM", ErrInvalidNotification)
	}
	plain, err := w.decryptResource(envelope.Resource.Ciphertext, envelope.Resource.Nonce, envelope.Resource.AssociatedData)
	if err != nil {
		return nil, err
	}
	var tx wechatTransaction
	if err := json.Unmarshal(plain, &tx); err != nil {
		return nil, fmt.Errorf("%w: WeChat transaction is not JSON", ErrInvalidNotification)
	}
	if tx.AppID != w.appID || tx.MchID != w.mchID {
		return nil, fmt.Errorf("%w: WeChat appid/mchid", ErrMerchantMismatch)
	}
	if tx.TradeState != "SUCCESS" {
		return nil, fmt.Errorf("%w: WeChat success event has trade_state %q", ErrInvalidNotification, tx.TradeState)
	}
	if tx.TransactionID == "" || len(tx.TransactionID) > 64 {
		return nil, fmt.Errorf("%w: WeChat transaction_id", ErrInvalidNotification)
	}
	if !validMerchantOrderNo(tx.OutTradeNo) {
		return nil, fmt.Errorf("%w: WeChat out_trade_no", ErrInvalidNotification)
	}
	if tx.Amount.Currency != CurrencyCNY {
		return nil, fmt.Errorf("%w: WeChat currency %q", ErrCurrencyMismatch, tx.Amount.Currency)
	}
	amount := Fen(tx.Amount.Total)
	if !validAmount(amount) {
		return nil, fmt.Errorf("%w: WeChat total %d fen", ErrInvalidNotification, amount)
	}
	var paidAt time.Time
	if tx.SuccessTime != "" {
		if t, err := time.Parse(time.RFC3339, tx.SuccessTime); err == nil {
			paidAt = t
		}
	}
	return &Notification{
		Provider:        ProviderWeChat,
		EventID:         envelope.ID,
		EventType:       envelope.EventType,
		MerchantOrderNo: tx.OutTradeNo,
		ProviderTradeNo: tx.TransactionID,
		AmountFen:       amount,
		Currency:        tx.Amount.Currency,
		State:           PaymentSucceeded,
		PaidAt:          paidAt,
		PayloadSHA256:   sha256Hex(body),
	}, nil
}

// decryptResource opens WeChat's AEAD_AES_256_GCM resource. Both a bad tag
// and malformed base64 fail closed.
func (w *WeChat) decryptResource(ciphertext, nonce, associatedData string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("%w: WeChat resource ciphertext", ErrInvalidNotification)
	}
	block, err := aes.NewCipher(w.apiV3Key)
	if err != nil {
		return nil, fmt.Errorf("%w: WeChat API v3 key", ErrInvalidConfig)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: WeChat GCM", ErrInvalidConfig)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("%w: WeChat resource nonce", ErrInvalidNotification)
	}
	plain, err := gcm.Open(nil, []byte(nonce), raw, []byte(associatedData))
	if err != nil {
		return nil, fmt.Errorf("%w: WeChat resource authentication", ErrInvalidSignature)
	}
	return plain, nil
}

func wechatSignRequest(key *rsa.PrivateKey, method, path, timestamp, nonce string, body []byte) (string, error) {
	message := method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	return signSHA256RSA(key, message)
}

func wechatNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("payment: wechat nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// parseWeChatPlatformCertificate reads the first certificate in a PEM
// bundle. The deployment supplies the current and next platform certificates
// during rotation.
func parseWeChatPlatformCertificate(text string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: WeChat platform certificate is not PEM", ErrInvalidConfig)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: WeChat platform certificate: %v", ErrInvalidConfig, err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("%w: WeChat platform certificate must hold an RSA key of at least %d bits", ErrInvalidConfig, minRSAKeyBits)
	}
	return cert, nil
}

// normalizeCertSerial makes the certificate's serial and the
// Wechatpay-Serial header comparable: upper-case hex without leading zeros.
func normalizeCertSerial(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimLeft(s, "0")
	return s
}

func wechatAPIError(body []byte) string {
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err == nil && (out.Code != "" || out.Message != "") {
		return trimForError(strings.TrimSpace(out.Code + ": " + out.Message))
	}
	return trimForError(string(body))
}
