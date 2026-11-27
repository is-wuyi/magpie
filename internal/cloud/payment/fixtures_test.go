package payment

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// testNow is the clock every fixture uses. Keeping it fixed makes room for
// both sides of callback-skew and expiry checks.
var testNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

const (
	testAlipayAppID      = "2026000000000001"
	testAlipaySellerID   = "2088000000000001"
	testWeChatAppID      = "wx0000000000000001"
	testWeChatMchID      = "1900000001"
	testWeChatMerchantSN = "5157F09EFDC096DE15EBE81A47057A7232F1B8E1"
	testWeChatAPIv3Key   = "0123456789abcdef0123456789abcdef"
	testWeChatSerial     = "5157F09EFDC096DE15EBE81A47057A7232F1B8E1"
)

type testKeys struct {
	alipayApp      *rsa.PrivateKey
	alipayProvider *rsa.PrivateKey
	wechatMerchant *rsa.PrivateKey
	wechatPlatform *rsa.PrivateKey
}

var (
	testKeysOnce sync.Once
	testKeySet   testKeys
)

func keysForTest(t *testing.T) testKeys {
	t.Helper()
	testKeysOnce.Do(func() {
		testKeySet = testKeys{
			alipayApp:      mustRSAKey(),
			alipayProvider: mustRSAKey(),
			wechatMerchant: mustRSAKey(),
			wechatPlatform: mustRSAKey(),
		}
	})
	return testKeySet
}

func mustRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

func privateKeyPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func publicKeyPEM(t *testing.T, key *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// platformCertPEM makes a self-signed certificate valid around testNow and
// returns its PEM and the normalized serial the callback header must carry.
func platformCertPEM(t *testing.T, key *rsa.PrivateKey, serialHex string) (string, string) {
	t.Helper()
	serial, ok := new(big.Int).SetString(serialHex, 16)
	if !ok {
		t.Fatalf("serial %q is not hex", serialHex)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "wechatpay test platform"},
		NotBefore:             testNow.Add(-24 * time.Hour),
		NotAfter:              testNow.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), normalizeCertSerial(cert.SerialNumber.Text(16))
}

// signAlipay is an independent implementation of Alipay's canonical string:
// non-empty parameters except the named ones, sorted, joined with &. It does
// not call the package's own canonicalizer, so a bug there fails a test.
func signAlipay(t *testing.T, key *rsa.PrivateKey, values url.Values, exclude ...string) string {
	t.Helper()
	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k] = true
	}
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(values))
	for k, list := range values {
		if skip[k] || len(list) != 1 || list[0] == "" {
			continue
		}
		pairs = append(pairs, pair{k, list[0]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	sig, err := signSHA256RSA(key, strings.Join(parts, "&"))
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func verifyAlipayURL(t *testing.T, key *rsa.PublicKey, rawURL string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	values := u.Query()
	signature := values.Get("sign")
	if signature == "" {
		t.Fatal("page pay URL has no sign")
	}
	if got := values.Get("sign_type"); got != "RSA2" {
		t.Fatalf("sign_type = %q, want RSA2", got)
	}
	unsigned := url.Values{}
	for k, v := range values {
		if k == "sign" {
			continue
		}
		unsigned[k] = v
	}
	if err := verifySHA256RSA(key, independentCanonical(unsigned, "sign"), signature); err != nil {
		t.Fatalf("page pay signature: %v", err)
	}
	return values
}

func independentCanonical(values url.Values, exclude ...string) string {
	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k] = true
	}
	keys := make([]string, 0, len(values))
	for k, list := range values {
		if skip[k] || len(list) != 1 || list[0] == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+values.Get(k))
	}
	return strings.Join(parts, "&")
}

func alipayNotifyForm(t *testing.T, key *rsa.PrivateKey, overrides map[string]string) url.Values {
	t.Helper()
	values := url.Values{
		"notify_time":  {"2026-11-26 12:00:00"},
		"notify_type":  {"trade_status_sync"},
		"notify_id":    {"ALI-EVENT-1"},
		"app_id":       {testAlipayAppID},
		"charset":      {"utf-8"},
		"version":      {"1.0"},
		"sign_type":    {"RSA2"},
		"trade_no":     {"2026112622001400000000000001"},
		"out_trade_no": {"MTEST000001"},
		"trade_status": {"TRADE_SUCCESS"},
		"total_amount": {"1.00"},
		"seller_id":    {testAlipaySellerID},
		"gmt_payment":  {"2026-11-26 12:00:00"},
		"gmt_create":   {"2026-11-26 11:59:00"},
	}
	for k, v := range overrides {
		if v == "" {
			values.Del(k)
			continue
		}
		values.Set(k, v)
	}
	values.Set("sign", signAlipay(t, key, values, "sign", "sign_type"))
	return values
}

type fakeHTTPClient struct {
	mu       sync.Mutex
	method   string
	url      string
	headers  http.Header
	body     []byte
	status   int
	response string
	err      error
}

func (f *fakeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.method = req.Method
	f.url = req.URL.String()
	f.headers = req.Header.Clone()
	f.body = body
	status, response, fnErr := f.status, f.response, f.err
	f.mu.Unlock()
	if fnErr != nil {
		return nil, fnErr
	}
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d test", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response)),
	}, nil
}

func (f *fakeHTTPClient) recorded(t *testing.T) (method, rawURL string, headers http.Header, body []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.method, f.url, f.headers.Clone(), append([]byte(nil), f.body...)
}

func parseWeChatAuthorization(t *testing.T, header string) map[string]string {
	t.Helper()
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || scheme != wechatAuthScheme {
		t.Fatalf("Authorization = %q, want scheme %s", header, wechatAuthScheme)
	}
	out := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			t.Fatalf("Authorization part %q", part)
		}
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

// wechatEncrypt seals a transaction resource exactly as WeChat's
// AEAD_AES_256_GCM resource does, with a fixed nonce for determinism.
func wechatEncrypt(t *testing.T, apiV3Key, associatedData string, plain []byte) (ciphertext, nonce string) {
	t.Helper()
	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce = "0123456789ab"
	sealed := gcm.Seal(nil, []byte(nonce), plain, []byte(associatedData))
	return base64.StdEncoding.EncodeToString(sealed), nonce
}

type wechatCallbackOptions struct {
	eventID       string
	eventType     string
	originalType  string
	headerSerial  string
	timestamp     time.Time
	nonce         string
	appID         string
	mchID         string
	outTradeNo    string
	transactionID string
	tradeState    string
	currency      string
	amountFen     int64
	successTime   string
	tamperBody    bool
}

func wechatCallback(t *testing.T, key *rsa.PrivateKey, opts wechatCallbackOptions) (http.Header, []byte) {
	t.Helper()
	if opts.eventID == "" {
		opts.eventID = "EV-20261126120000000001"
	}
	if opts.eventType == "" {
		opts.eventType = "TRANSACTION.SUCCESS"
	}
	if opts.originalType == "" {
		opts.originalType = "transaction"
	}
	if opts.headerSerial == "" {
		opts.headerSerial = testWeChatSerial
	}
	if opts.timestamp.IsZero() {
		opts.timestamp = testNow
	}
	if opts.nonce == "" {
		opts.nonce = "test-nonce"
	}
	if opts.appID == "" {
		opts.appID = testWeChatAppID
	}
	if opts.mchID == "" {
		opts.mchID = testWeChatMchID
	}
	if opts.outTradeNo == "" {
		opts.outTradeNo = "MTEST000001"
	}
	if opts.transactionID == "" {
		opts.transactionID = "4200000000202611260000000001"
	}
	if opts.tradeState == "" {
		opts.tradeState = "SUCCESS"
	}
	if opts.currency == "" {
		opts.currency = CurrencyCNY
	}
	if opts.amountFen == 0 {
		opts.amountFen = 100
	}
	if opts.successTime == "" {
		opts.successTime = "2026-11-26T11:59:30+08:00"
	}
	tx := map[string]any{
		"appid":            opts.appID,
		"mchid":            opts.mchID,
		"out_trade_no":     opts.outTradeNo,
		"transaction_id":   opts.transactionID,
		"trade_type":       "NATIVE",
		"trade_state":      opts.tradeState,
		"success_time":     opts.successTime,
		"payer":            map[string]any{"openid": "oUpF8uMuAJO_M2pxb1Q9zNjWeS6o"},
		"amount":           map[string]any{"total": opts.amountFen, "payer_total": opts.amountFen, "currency": opts.currency, "payer_currency": opts.currency},
		"promotion_detail": []any{},
	}
	plain, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce := wechatEncrypt(t, testWeChatAPIv3Key, "transaction", plain)
	envelope := map[string]any{
		"id":            opts.eventID,
		"create_time":   "2026-11-26T12:00:00+08:00",
		"resource_type": "encrypt-resource",
		"event_type":    opts.eventType,
		"summary":       "支付成功",
		"resource": map[string]any{
			"original_type":   opts.originalType,
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      ciphertext,
			"associated_data": "transaction",
			"nonce":           nonce,
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if opts.tamperBody {
		body = append([]byte(nil), body...)
		body[len(body)/2] ^= 0x01
	}
	timestamp := fmt.Sprintf("%d", opts.timestamp.Unix())
	sig, err := signSHA256RSA(key, timestamp+"\n"+opts.nonce+"\n"+string(body)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Wechatpay-Timestamp", timestamp)
	header.Set("Wechatpay-Nonce", opts.nonce)
	header.Set("Wechatpay-Signature", sig)
	header.Set("Wechatpay-Serial", opts.headerSerial)
	header.Set("Wechatpay-Signature-Type", wechatAuthScheme)
	return header, body
}

// memStore is an in-memory Store with the same row-lock and rollback
// behaviour the callback path relies on. All transactions run one at a time,
// as a row lock would make them.
type memStore struct {
	mu     sync.Mutex
	orders map[string]*Order
	events map[string]*Event
}

func newMemStore() *memStore {
	return &memStore{orders: map[string]*Order{}, events: map[string]*Event{}}
}

func (m *memStore) InTx(ctx context.Context, fn func(context.Context, Tx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	orders := make(map[string]*Order, len(m.orders))
	for k, v := range m.orders {
		orders[k] = v.clone()
	}
	events := make(map[string]*Event, len(m.events))
	for k, v := range m.events {
		events[k] = cloneEvent(v)
	}
	if err := fn(ctx, &memTx{store: m}); err != nil {
		m.orders, m.events = orders, events
		return err
	}
	return nil
}

func (m *memStore) OrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[merchantOrderNo]
	if !ok {
		return nil, ErrOrderNotFound
	}
	return o.clone(), nil
}

func (m *memStore) PendingCreditOrders(ctx context.Context, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, o := range m.orders {
		if o.Status == StatusSuccess && o.CreditState == CreditPending {
			out = append(out, o.MerchantOrderNo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) countEvents() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func (m *memStore) event(provider Provider, eventID string) (*Event, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.events[eventKey(provider, eventID)]
	return cloneEvent(e), ok
}

type memTx struct {
	store *memStore
}

func (t *memTx) InsertOrder(ctx context.Context, o *Order) error {
	if err := o.validate(); err != nil {
		return err
	}
	if _, dup := t.store.orders[o.MerchantOrderNo]; dup {
		return fmt.Errorf("%w: duplicate merchant order %s", ErrInvalidOrder, o.MerchantOrderNo)
	}
	t.store.orders[o.MerchantOrderNo] = o.clone()
	return nil
}

func (t *memTx) LockOrderByMerchantNo(ctx context.Context, merchantOrderNo string) (*Order, error) {
	o, ok := t.store.orders[merchantOrderNo]
	if !ok {
		return nil, ErrOrderNotFound
	}
	return o.clone(), nil
}

func (t *memTx) SaveOrder(ctx context.Context, o *Order) error {
	if _, ok := t.store.orders[o.MerchantOrderNo]; !ok {
		return ErrOrderNotFound
	}
	if o.ProviderTradeNo != "" {
		for no, other := range t.store.orders {
			if no == o.MerchantOrderNo {
				continue
			}
			if other.Provider == o.Provider && other.ProviderTradeNo == o.ProviderTradeNo {
				return fmt.Errorf("%w: provider trade %s already pays %s", ErrTradeNoMismatch, o.ProviderTradeNo, no)
			}
		}
	}
	t.store.orders[o.MerchantOrderNo] = o.clone()
	return nil
}

func (t *memTx) InsertEvent(ctx context.Context, e *Event) (bool, error) {
	key := eventKey(e.Provider, e.ProviderEventID)
	if _, ok := t.store.events[key]; ok {
		return false, nil
	}
	t.store.events[key] = cloneEvent(e)
	return true, nil
}

func (t *memTx) MarkCreditPendingEventsCredited(ctx context.Context, orderID uuid.UUID) error {
	for _, e := range t.store.events {
		if e.OrderID == orderID && e.Outcome == EventCreditPending {
			e.Outcome = EventCredited
		}
	}
	return nil
}

func (t *memTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("memStore: raw SQL is not supported")
}

func (t *memTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return errRow{err: fmt.Errorf("memStore: raw SQL is not supported")}
}

func (t *memTx) Raw() pgx.Tx { return nil }

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

func eventKey(provider Provider, eventID string) string {
	return string(provider) + "\x00" + eventID
}

func cloneEvent(e *Event) *Event {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// fakeCreditor records ledger effects by idempotency key, like the wallet's
// unique key does.
type fakeCreditor struct {
	mu       sync.Mutex
	calls    int
	failures int
	ledger   map[string]int64
	order    []string
}

func newFakeCreditor() *fakeCreditor {
	return &fakeCreditor{ledger: map[string]int64{}}
}

func (f *fakeCreditor) Credit(ctx context.Context, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errors.New("fake wallet is unavailable")
	}
	if _, ok := f.ledger[idempotencyKey]; !ok {
		f.ledger[idempotencyKey] = amountMicroCNY
		f.order = append(f.order, idempotencyKey)
	}
	return nil
}

func (f *fakeCreditor) balance() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, v := range f.ledger {
		total += v
	}
	return total
}

func (f *fakeCreditor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCreditor) failNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = n
}

// txCreditor implements both Creditor (by embedding) and
// TransactionalCreditor. fail makes the in-transaction call fail before the
// ledger changes, which the service must roll back.
type txCreditor struct {
	*fakeCreditor
	fail bool
}

func (c *txCreditor) CreditTx(ctx context.Context, tx Tx, userUUID uuid.UUID, amountMicroCNY int64, idempotencyKey, description string) error {
	if c.fail {
		return errors.New("fake wallet transaction is unavailable")
	}
	return c.Credit(ctx, userUUID, amountMicroCNY, idempotencyKey, description)
}

// testEnv wires a Service to an in-memory store, a fake wallet and both real
// provider clients using in-test keys.
type testEnv struct {
	svc      *Service
	store    *memStore
	creditor *fakeCreditor
	keys     testKeys
	alipay   *Alipay
	wechat   *WeChat
	http     *fakeHTTPClient
	seq      int
	now      time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWith(t, nil, nil)
}

func newTestEnvWith(t *testing.T, creditor Creditor, modify func(*Config)) *testEnv {
	t.Helper()
	keys := keysForTest(t)
	certPEM, _ := platformCertPEM(t, keys.wechatPlatform, testWeChatSerial)
	alipay, err := NewAlipay(AlipayConfig{
		AppID:              testAlipayAppID,
		PrivateKeyPEM:      privateKeyPEM(t, keys.alipayApp),
		AlipayPublicKeyPEM: publicKeyPEM(t, &keys.alipayProvider.PublicKey),
		SellerID:           testAlipaySellerID,
		NotifyURL:          "https://shop.example.test/alipay/notify",
		ReturnURL:          "https://shop.example.test/alipay/return",
		Now:                func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	fakeHTTP := &fakeHTTPClient{response: `{"code_url":"weixin://wxpay/bizpayurl?pr=test123"}`}
	wechat, err := NewWeChat(WeChatConfig{
		AppID:                testWeChatAppID,
		MchID:                testWeChatMchID,
		MerchantSerialNo:     testWeChatMerchantSN,
		PrivateKeyPEM:        privateKeyPEM(t, keys.wechatMerchant),
		APIv3Key:             testWeChatAPIv3Key,
		NotifyURL:            "https://shop.example.test/wechat/notify",
		PlatformCertificates: []string{certPEM},
		HTTPClient:           fakeHTTP,
		Now:                  func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	if creditor == nil {
		creditor = newFakeCreditor()
	}
	env := &testEnv{
		store:  store,
		keys:   keys,
		alipay: alipay,
		wechat: wechat,
		http:   fakeHTTP,
		now:    testNow,
	}
	switch c := creditor.(type) {
	case *fakeCreditor:
		env.creditor = c
	case *txCreditor:
		env.creditor = c.fakeCreditor
	}
	cfg := Config{
		Store:    store,
		Creditor: creditor,
		Presets: []Preset{
			{ID: "p1", AmountFen: 100, Label: "1元充值"},
			{ID: "p10", AmountFen: 1000, Label: "10元充值"},
		},
		Alipay:   alipay,
		WeChat:   wechat,
		OrderTTL: 30 * time.Minute,
		Now:      func() time.Time { return testNow },
		NewOrderID: func() uuid.UUID {
			env.seq++
			return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", env.seq))
		},
		NewMerchantOrderNo: func(now time.Time) (string, error) {
			env.seq++
			return fmt.Sprintf("MTEST%06d", env.seq), nil
		},
	}
	if modify != nil {
		modify(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.svc = svc
	return env
}

func (e *testEnv) createOrder(t *testing.T, provider Provider) Checkout {
	t.Helper()
	checkout, err := e.svc.CreateOrder(context.Background(), uuid.MustParse("11111111-1111-4111-8111-111111111111"), provider, "p1")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	return checkout
}

func (e *testEnv) alipayPaidNotify(t *testing.T, order Order) url.Values {
	t.Helper()
	return alipayNotifyForm(t, e.keys.alipayProvider, map[string]string{
		"out_trade_no": order.MerchantOrderNo,
		"total_amount": order.AmountFen.YuanString(),
		"notify_id":    "ALI-" + order.MerchantOrderNo,
	})
}

func (e *testEnv) wechatPaidCallback(t *testing.T, order Order) (http.Header, []byte) {
	t.Helper()
	return wechatCallback(t, e.keys.wechatPlatform, wechatCallbackOptions{
		outTradeNo: order.MerchantOrderNo,
		amountFen:  int64(order.AmountFen),
	})
}
