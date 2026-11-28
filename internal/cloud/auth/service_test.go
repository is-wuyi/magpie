package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/cloud/store"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

var testNowTime = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func newTestService(t *testing.T, repo Repository, mailer Mailer, tweak func(*Config)) *Service {
	t.Helper()
	cfg := Config{Secret: bytes.Repeat([]byte{0x5a}, 32)}
	if tweak != nil {
		tweak(&cfg)
	}
	svc, err := New(cfg, repo, mailer)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return testNowTime }
	return svc
}

func mustRequestCode(t *testing.T, svc *Service, mail *fakeMailer, email string) string {
	t.Helper()
	if _, err := svc.repo.EnsureVerifiedUser(context.Background(), store.EnsureVerifiedUserParams{
		Email: email, EmailNormalized: email, VerifiedAt: testNowTime, PromoteAdmin: svc.isAdmin(email),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: email, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	return mail.last().Code
}

func wrongCode(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: " User@Example.COM ", want: "user@example.com"},
		{in: "first.last+tag@example.co.uk", want: "first.last+tag@example.co.uk"},
		{in: "", wantErr: true},
		{in: "user@example.com\r\nBcc: x@example.com", wantErr: true},
		{in: "Name <user@example.com>", wantErr: true},
		{in: "user@example", wantErr: true},
		{in: "user@.example.com", wantErr: true},
		{in: "user@example..com", wantErr: true},
		{in: "user@@example.com", wantErr: true},
		{in: strings.Repeat("a", 250) + "@example.com", wantErr: true},
	}
	for _, c := range cases {
		got, err := NormalizeEmail(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: got %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	valid := func() *Service { return newTestService(t, repo, mail, nil) }
	svc := valid()
	if svc.cfg.CodeTTL != DefaultCodeTTL || svc.cfg.SessionTTL != DefaultSessionTTL || svc.cfg.CookieName != DefaultCookieName || svc.cfg.CookieSameSite != http.SameSiteLaxMode {
		t.Fatalf("defaults not applied: %+v", svc.cfg)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "short secret", cfg: Config{Secret: []byte("short")}},
		{name: "bad admin", cfg: Config{Secret: bytes.Repeat([]byte{1}, 32), AdminEmails: []string{"not an email"}}},
		{name: "bad origin", cfg: Config{Secret: bytes.Repeat([]byte{1}, 32), TrustedOrigins: []string{"example.com"}}},
		{name: "bad cookie", cfg: Config{Secret: bytes.Repeat([]byte{1}, 32), CookieName: "bad name"}},
		{name: "none samesite", cfg: Config{Secret: bytes.Repeat([]byte{1}, 32), CookieSameSite: http.SameSiteNoneMode}},
		{name: "zero attempts", cfg: Config{Secret: bytes.Repeat([]byte{1}, 32), CodeMaxAttempts: -1}},
	}
	for _, c := range cases {
		if _, err := New(c.cfg, repo, mail); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: got %v, want ErrInvalidConfig", c.name, err)
		}
	}
	if _, err := New(Config{Secret: bytes.Repeat([]byte{1}, 32)}, nil, mail); err == nil {
		t.Error("nil repository accepted")
	}
	if _, err := New(Config{Secret: bytes.Repeat([]byte{1}, 32)}, repo, nil); err == nil {
		t.Error("nil mailer accepted")
	}
}

func TestConfigFromEnv(t *testing.T) {
	for _, name := range []string{
		"MAGPIE_AUTH_SECRET", "MAGPIE_AUTH_COOKIE_NAME", "MAGPIE_AUTH_COOKIE_DOMAIN",
		"MAGPIE_ADMIN_EMAILS", "MAGPIE_AUTH_TRUSTED_ORIGINS", "MAGPIE_AUTH_COOKIE_INSECURE",
		"MAGPIE_AUTH_TRUST_PROXY_HEADERS", "MAGPIE_AUTH_CODE_TTL", "MAGPIE_AUTH_SEND_INTERVAL",
		"MAGPIE_AUTH_SESSION_TTL", "MAGPIE_AUTH_SESSION_TOUCH_INTERVAL", "MAGPIE_AUTH_CODE_MAX_ATTEMPTS",
		"MAGPIE_AUTH_SENDS_PER_HOUR", "MAGPIE_AUTH_SENDS_PER_IP_PER_HOUR",
		"MAGPIE_AUTH_VERIFY_FAILURES_PER_HOUR", "MAGPIE_AUTH_VERIFY_FAILURES_PER_IP_PER_HOUR",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("MAGPIE_AUTH_SECRET", strings.Repeat("s", 32))
	t.Setenv("MAGPIE_ADMIN_EMAILS", " Admin@Example.com, second@example.com ")
	t.Setenv("MAGPIE_AUTH_TRUSTED_ORIGINS", "https://magpie.example")
	t.Setenv("MAGPIE_AUTH_CODE_TTL", "7m")
	t.Setenv("MAGPIE_AUTH_SENDS_PER_HOUR", "9")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.Secret) != strings.Repeat("s", 32) || cfg.CodeTTL != 7*time.Minute || cfg.SendsPerHour != 9 {
		t.Fatalf("got %+v", cfg)
	}
	if len(cfg.AdminEmails) != 2 || cfg.AdminEmails[0] != "Admin@Example.com" {
		t.Fatalf("admin list: %#v", cfg.AdminEmails)
	}
	if len(cfg.TrustedOrigins) != 1 || cfg.TrustedOrigins[0] != "https://magpie.example" {
		t.Fatalf("trusted origins: %#v", cfg.TrustedOrigins)
	}

	t.Setenv("MAGPIE_AUTH_COOKIE_INSECURE", "sometimes")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("malformed boolean accepted")
	}
	t.Setenv("MAGPIE_AUTH_COOKIE_INSECURE", "")
	t.Setenv("MAGPIE_AUTH_SENDS_PER_HOUR", "many")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("malformed integer accepted")
	}
	t.Setenv("MAGPIE_AUTH_SENDS_PER_HOUR", "")
	t.Setenv("MAGPIE_AUTH_CODE_TTL", "ten minutes")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("malformed duration accepted")
	}
}

func TestRequestLoginCode(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	sent, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: " User@Example.COM ", IP: "203.0.113.9"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.RetryAfter != DefaultSendInterval {
		t.Fatalf("retry after: %v", sent.RetryAfter)
	}
	msg := mail.last()
	if msg.To != "user@example.com" || msg.Purpose != PurposeLogin || !msg.ExpiresAt.Equal(testNowTime.Add(DefaultCodeTTL)) {
		t.Fatalf("message: %+v", msg)
	}
	if !isSixDigits(msg.Code) {
		t.Fatalf("code %q is not six digits", msg.Code)
	}
	p := repo.lastIssue
	if p.EmailNormalized != "user@example.com" || p.Purpose != PurposeLogin || p.HashVersion != codeHashVersion ||
		p.MaxAttempts != DefaultCodeMaxAttempts || !p.Now.Equal(testNowTime) || !p.ExpiresAt.Equal(testNowTime.Add(DefaultCodeTTL)) ||
		p.RequestIP != "203.0.113.9" {
		t.Fatalf("issue params: %+v", p)
	}
	if len(p.CodeHash) != 32 {
		t.Fatalf("hash length %d", len(p.CodeHash))
	}
	if !bytes.Equal(p.CodeHash, svc.hashLoginCode(PurposeLogin, "user@example.com", msg.Code)) {
		t.Fatal("stored hash is not the HMAC of the code")
	}
	if bytes.Contains(p.CodeHash, []byte(msg.Code)) {
		t.Fatal("the six digits are present in the stored hash")
	}
	if repo.userCount() != 0 {
		t.Fatal("requesting a code created a user")
	}
}

func TestRequestLoginCodeMailFailureDiscardsCode(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	mail.err = errFakeDB
	_, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: "fail@example.com"})
	if !errors.Is(err, ErrMailFailed) {
		t.Fatalf("got %v, want ErrMailFailed", err)
	}
	code := mail.last().Code
	if strings.Contains(err.Error(), code) {
		t.Fatal("the error carries the code")
	}
	c := repo.onlyCode()
	if len(repo.deleted) != 1 || repo.deleted[0] != c.ID {
		t.Fatalf("code was not discarded: %+v", repo.deleted)
	}
}

func TestRequestLoginCodeRateLimited(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	repo.issueErr = &store.RateLimitError{RetryAfter: 42 * time.Second}
	svc := newTestService(t, repo, mail, nil)
	_, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: "rate@example.com"})
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 42*time.Second {
		t.Fatalf("got %v, want a 42s RateLimitError", err)
	}
	if !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("got %v, want ErrTooManyRequests", err)
	}
	if mail.count() != 0 {
		t.Fatal("a rate-limited request mailed a code")
	}
}

func TestVerifyLoginCodeSuccess(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "user@example.com")

	result, err := svc.VerifyLoginCode(context.Background(), LoginRequest{
		Email: "USER@example.com", Code: code, IP: "203.0.113.9", UserAgent: "test-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Role != store.RoleUser || result.User.Email != "user@example.com" || result.User.EmailVerifiedAt == nil {
		t.Fatalf("user: %+v", result.User)
	}
	if result.Token == "" || !result.ExpiresAt.Equal(testNowTime.Add(DefaultSessionTTL)) {
		t.Fatalf("session result: expires %v token %q", result.ExpiresAt, result.Token)
	}
	if !bytes.Equal(result.Session.TokenHash, svc.hashSessionToken(result.Token)) {
		t.Fatal("session stores something other than the token HMAC")
	}
	if string(result.Session.TokenHash) == result.Token {
		t.Fatal("session stores the raw token")
	}
	if repo.userCount() != 1 || repo.sessionCount() != 1 {
		t.Fatalf("users %d sessions %d", repo.userCount(), repo.sessionCount())
	}
	if repo.lastEnsure.PromoteAdmin {
		t.Fatal("a normal address was promoted")
	}
	p, err := svc.Authenticate(context.Background(), result.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != result.User.ID || p.SessionID != result.Session.ID || p.Email != "user@example.com" || p.Role != store.RoleUser {
		t.Fatalf("principal: %+v", p)
	}
}

func TestVerifyLoginCodeAdminOnlyAfterVerification(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, func(cfg *Config) {
		cfg.AdminEmails = []string{" Admin@Example.com "}
	})
	code := mustRequestCode(t, svc, mail, "admin@example.com")
	result, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "admin@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Role != store.RoleAdmin || !repo.lastEnsure.PromoteAdmin {
		t.Fatalf("admin was not promoted: %+v", result.User)
	}

	// A different address with the same code cannot be promoted: the code
	// is bound to its address.
	otherRepo, otherMail := newFakeRepo(), &fakeMailer{}
	other := newTestService(t, otherRepo, otherMail, func(cfg *Config) {
		cfg.AdminEmails = []string{"admin@example.com"}
	})
	otherCode := mustRequestCode(t, other, otherMail, "someone@example.com")
	if _, err := other.VerifyLoginCode(context.Background(), LoginRequest{Email: "admin@example.com", Code: otherCode}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("a code for another address was accepted: %v", err)
	}
}

func TestVerifyLoginCodeWrongThenRight(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "wrong@example.com")

	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "wrong@example.com", Code: wrongCode(code)}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if repo.onlyCode().Attempts != 1 {
		t.Fatalf("attempts: %d", repo.onlyCode().Attempts)
	}
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "wrong@example.com", Code: code}); err != nil {
		t.Fatalf("right code after one wrong: %v", err)
	}
}

func TestVerifyLoginCodeRejectsMalformedBeforeStorage(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	for _, code := range []string{"", "12345", "1234567", "abcdef", "12 456"} {
		if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "shape@example.com", Code: code}); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("%q: got %v, want ErrInvalidCode", code, err)
		}
	}
	if repo.countsCalls != 0 || repo.latestCalls != 0 {
		t.Fatalf("malformed codes reached storage: counts %d latest %d", repo.countsCalls, repo.latestCalls)
	}
}

func TestVerifyLoginCodeExhaustsAttempts(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, func(cfg *Config) { cfg.CodeMaxAttempts = 2 })
	code := mustRequestCode(t, svc, mail, "exhaust@example.com")
	for i := 0; i < 2; i++ {
		if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "exhaust@example.com", Code: wrongCode(code)}); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if repo.onlyCode().Attempts != 2 || repo.onlyCode().ConsumedAt == nil {
		t.Fatalf("code not exhausted: %+v", repo.onlyCode())
	}
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "exhaust@example.com", Code: code}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("exhausted code accepted: %v", err)
	}
}

func TestVerifyLoginCodeExpired(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "expired@example.com")
	svc.now = func() time.Time { return testNowTime.Add(DefaultCodeTTL + time.Second) }
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "expired@example.com", Code: code}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code accepted: %v", err)
	}
}

func TestVerifyLoginCodeIsOneTime(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "once@example.com")
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "once@example.com", Code: code}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "once@example.com", Code: code}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code accepted twice: %v", err)
	}
}

func TestVerifyLoginCodeThrottle(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	repo.counts = &store.FailureCounts{ByEmail: DefaultVerifyFailuresPerHour}
	svc := newTestService(t, repo, mail, nil)
	_, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "throttle@example.com", Code: "123456", IP: "203.0.113.9"})
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != throttleWindow {
		t.Fatalf("email throttle: %v", err)
	}
	if repo.latestCalls != 0 {
		t.Fatal("a throttled verify read a code")
	}

	repo2, mail2 := newFakeRepo(), &fakeMailer{}
	repo2.counts = &store.FailureCounts{ByIP: DefaultVerifyFailuresPerIPPerHour}
	svc2 := newTestService(t, repo2, mail2, nil)
	if _, err := svc2.VerifyLoginCode(context.Background(), LoginRequest{Email: "ipthrottle@example.com", Code: "123456", IP: "203.0.113.9"}); !errors.As(err, &limited) {
		t.Fatalf("IP throttle: %v", err)
	}
}

func TestVerifyLoginCodeStorageFailureFailsClosed(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "storage@example.com")

	repo.countsErr = errFakeDB
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "storage@example.com", Code: code}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("counts failure: %v", err)
	}
	repo.countsErr = nil

	repo.latestErr = errFakeDB
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "storage@example.com", Code: code}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lookup failure: %v", err)
	}
	repo.latestErr = nil

	repo.recordErr = errFakeDB
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "storage@example.com", Code: wrongCode(code)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failure record failure: %v", err)
	}
	repo.recordErr = nil

	repo.consumeErr = errFakeDB
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "storage@example.com", Code: code}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("consume failure: %v", err)
	}
}

func TestVerifyLoginCodeAccountStates(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "disabled@example.com")
	id := repo.byEmail["disabled@example.com"]
	u := repo.users[id]
	u.Status = store.StatusDisabled
	repo.users[id] = u
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "disabled@example.com", Code: code}); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("disabled: %v", err)
	}

	repo2, mail2 := newFakeRepo(), &fakeMailer{}
	svc2 := newTestService(t, repo2, mail2, nil)
	code2 := mustRequestCode(t, svc2, mail2, "stale@example.com")
	repo2.createErr = store.ErrSessionStale
	if _, err := svc2.VerifyLoginCode(context.Background(), LoginRequest{Email: "stale@example.com", Code: code2}); !errors.Is(err, ErrSignInFailed) {
		t.Fatalf("stale session: %v", err)
	}
}

func TestSessionCookie(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	expires := testNowTime.Add(time.Hour)
	cookie := svc.SessionCookie("token-value", expires)
	if cookie.Name != DefaultCookieName || cookie.Value != "token-value" || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != 3600 {
		t.Fatalf("cookie: %+v", cookie)
	}
	clear := svc.ClearSessionCookie()
	if clear.Value != "" || clear.MaxAge != -1 || !clear.HttpOnly || !clear.Secure || clear.SameSite != http.SameSiteLaxMode {
		t.Fatalf("clear cookie: %+v", clear)
	}

	insecure := newTestService(t, repo, mail, func(cfg *Config) { cfg.AllowInsecureCookie = true })
	if insecure.SessionCookie("t", expires).Secure {
		t.Fatal("AllowInsecureCookie did not drop Secure")
	}
	strict := newTestService(t, repo, mail, func(cfg *Config) { cfg.CookieSameSite = http.SameSiteStrictMode })
	if strict.SessionCookie("t", expires).SameSite != http.SameSiteStrictMode {
		t.Fatal("SameSite was not kept")
	}
}

func TestAuthenticateAndLogout(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "session@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "session@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Authenticate(context.Background(), login.Token); err != nil {
		t.Fatal(err)
	}
	if len(repo.touchCalls) != 0 {
		t.Fatal("a fresh session was touched")
	}
	svc.now = func() time.Time { return testNowTime.Add(6 * time.Minute) }
	if _, err := svc.Authenticate(context.Background(), login.Token); err != nil {
		t.Fatal(err)
	}
	if len(repo.touchCalls) != 1 {
		t.Fatalf("touch calls: %d", len(repo.touchCalls))
	}

	if err := svc.Logout(context.Background(), login.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), login.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
	if err := svc.Logout(context.Background(), login.Token); err != nil {
		t.Fatalf("repeated logout: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), strings.Repeat("x", maxTokenLength+1)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("oversized token: %v", err)
	}

	repo.sessionErr = errFakeDB
	if _, err := svc.Authenticate(context.Background(), "something"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("storage failure: %v", err)
	}
	repo.sessionErr = nil
	repo.revokeErr = errFakeDB
	if err := svc.Logout(context.Background(), login.Token); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("logout storage failure: %v", err)
	}
}

func TestLogoutAll(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "all@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "all@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Authenticate(context.Background(), login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.LogoutAll(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if repo.revokeAllCall != 1 {
		t.Fatalf("revoke all calls: %d", repo.revokeAllCall)
	}
	if _, err := svc.Authenticate(context.Background(), login.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("session survived logout all: %v", err)
	}
	if err := svc.LogoutAll(context.Background(), Principal{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("empty principal: %v", err)
	}
	repo.revokeAllErr = errFakeDB
	if err := svc.LogoutAll(context.Background(), p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("storage failure: %v", err)
	}
}

func TestTokenFromRequest(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	if _, ok := svc.TokenFromRequest(r); ok {
		t.Fatal("no cookie read as a token")
	}
	r.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: "abc"})
	if token, ok := svc.TokenFromRequest(r); !ok || token != "abc" {
		t.Fatalf("token: %q %v", token, ok)
	}
	r, _ = http.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: strings.Repeat("x", maxTokenLength+1)})
	if _, ok := svc.TokenFromRequest(r); ok {
		t.Fatal("oversized cookie accepted")
	}
}

func TestClientIP(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	if got := ClientIP(r, false); got != "203.0.113.9" {
		t.Fatalf("remote: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.1")
	if got := ClientIP(r, false); got != "203.0.113.9" {
		t.Fatalf("untrusted proxy header was read: %q", got)
	}
	if got := ClientIP(r, true); got != "198.51.100.1" {
		t.Fatalf("trusted proxy: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := ClientIP(r, true); got != "" {
		t.Fatalf("malformed forwarded header fell back to the proxy: %q", got)
	}
	r.Header.Del("X-Forwarded-For")
	r.Header.Set("X-Real-IP", "192.0.2.7")
	if got := ClientIP(r, true); got != "192.0.2.7" {
		t.Fatalf("X-Real-IP: %q", got)
	}
	r, _ = http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[2001:db8::1]:443"
	if got := ClientIP(r, false); got != "2001:db8::1" {
		t.Fatalf("IPv6: %q", got)
	}
}

func TestCodeHashBindsPurposeAndAddress(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	base := svc.hashLoginCode(PurposeLogin, "a@example.com", "123456")
	if bytes.Equal(base, svc.hashLoginCode(PurposeLogin, "b@example.com", "123456")) {
		t.Fatal("one code hash fits two addresses")
	}
	if bytes.Equal(base, svc.hashLoginCode("reset", "a@example.com", "123456")) {
		t.Fatal("one code hash fits two purposes")
	}
	if bytes.Equal(base, svc.hashLoginCode(PurposeLogin, "a@example.com", "654321")) {
		t.Fatal("one code hash fits two codes")
	}
	other := newTestService(t, newFakeRepo(), &fakeMailer{}, func(cfg *Config) {
		cfg.Secret = bytes.Repeat([]byte{0x11}, 32)
	})
	if bytes.Equal(base, other.hashLoginCode(PurposeLogin, "a@example.com", "123456")) {
		t.Fatal("the hash does not depend on the deployment secret")
	}
}

func TestRandomLoginCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		code, err := randomLoginCode()
		if err != nil {
			t.Fatal(err)
		}
		if !isSixDigits(code) {
			t.Fatalf("randomLoginCode gave %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 2 {
		t.Fatal("randomLoginCode repeated one value 50 times")
	}
}
