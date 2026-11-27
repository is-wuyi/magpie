// Package auth implements cloud sign-in: a six-digit code mailed to an
// address, an ordinary user created after that address is proven, and a
// random server-side session kept in a secure cookie.
//
// The package owns no storage and no SMTP client. It talks to a Repository
// (implemented by internal/cloud/store) and a Mailer (an SMTP sender, or a
// test fake), so its rules can be tested without a database or a network.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults chosen for a public deployment. A code is valid for ten
// minutes, a second send waits a minute, and a session lasts thirty days.
const (
	DefaultCodeTTL                    = 10 * time.Minute
	DefaultCodeMaxAttempts            = 5
	DefaultSendInterval               = time.Minute
	DefaultSendsPerHour               = 5
	DefaultSendsPerIPPerHour          = 20
	DefaultVerifyFailuresPerHour      = 10
	DefaultVerifyFailuresPerIPPerHour = 30
	DefaultSessionTTL                 = 30 * 24 * time.Hour
	DefaultSessionTouchInterval       = 5 * time.Minute
	DefaultCookieName                 = "magpie_session"
	DefaultLoginPurpose               = "login"

	minSecretLength = 32
)

// Config is the authentication policy. Zero values are replaced by the
// defaults above; Secret is the exception and is required.
type Config struct {
	// Secret keys the HMACs that protect email codes and session tokens.
	// It must be at least 32 bytes and should be the same on every instance
	// that shares a database. It is never sent to a client.
	Secret []byte

	CodeTTL         time.Duration
	CodeMaxAttempts int

	// SendInterval is the minimum wait between two sends to one address.
	// Zero takes the default; the hourly caps always apply.
	SendInterval      time.Duration
	SendsPerHour      int
	SendsPerIPPerHour int

	VerifyFailuresPerHour      int
	VerifyFailuresPerIPPerHour int

	SessionTTL           time.Duration
	SessionTouchInterval time.Duration

	CookieName   string
	CookieDomain string
	// AllowInsecureCookie drops the Secure flag. It exists for a local
	// http:// deployment only and must stay false in production.
	AllowInsecureCookie bool
	CookieSameSite      http.SameSite

	// TrustProxyHeaders makes ClientIP read X-Forwarded-For and X-Real-IP.
	// Set it only when a reverse proxy overwrites those headers.
	TrustProxyHeaders bool

	// TrustedOrigins are extra scheme://host origins a browser write may
	// come from. The request's own Host is always accepted; this is for a
	// proxy whose Host differs from the public address.
	TrustedOrigins []string

	// AdminEmails is the operator's explicit allow list. Matching is on the
	// normalized address, and a match promotes a user only after a code
	// sent to that address was verified. Removing an address from the list
	// does not demote an existing administrator.
	AdminEmails []string
}

// ConfigFromEnv reads the MAGPIE_AUTH_* variables. An unset value takes its
// default; a malformed one is an error, so a deployment never starts with a
// throttle silently turned off.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Secret:         []byte(envOr("MAGPIE_AUTH_SECRET", "")),
		CookieName:     envOr("MAGPIE_AUTH_COOKIE_NAME", ""),
		CookieDomain:   envOr("MAGPIE_AUTH_COOKIE_DOMAIN", ""),
		AdminEmails:    splitList(envOr("MAGPIE_ADMIN_EMAILS", "")),
		TrustedOrigins: splitList(envOr("MAGPIE_AUTH_TRUSTED_ORIGINS", "")),
	}
	var err error
	if cfg.AllowInsecureCookie, err = envBool("MAGPIE_AUTH_COOKIE_INSECURE"); err != nil {
		return Config{}, err
	}
	if cfg.TrustProxyHeaders, err = envBool("MAGPIE_AUTH_TRUST_PROXY_HEADERS"); err != nil {
		return Config{}, err
	}
	if cfg.CodeTTL, err = envDuration("MAGPIE_AUTH_CODE_TTL", 0); err != nil {
		return Config{}, err
	}
	if cfg.SendInterval, err = envDuration("MAGPIE_AUTH_SEND_INTERVAL", 0); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = envDuration("MAGPIE_AUTH_SESSION_TTL", 0); err != nil {
		return Config{}, err
	}
	if cfg.SessionTouchInterval, err = envDuration("MAGPIE_AUTH_SESSION_TOUCH_INTERVAL", 0); err != nil {
		return Config{}, err
	}
	if cfg.CodeMaxAttempts, err = envInt("MAGPIE_AUTH_CODE_MAX_ATTEMPTS", 0); err != nil {
		return Config{}, err
	}
	if cfg.SendsPerHour, err = envInt("MAGPIE_AUTH_SENDS_PER_HOUR", 0); err != nil {
		return Config{}, err
	}
	if cfg.SendsPerIPPerHour, err = envInt("MAGPIE_AUTH_SENDS_PER_IP_PER_HOUR", 0); err != nil {
		return Config{}, err
	}
	if cfg.VerifyFailuresPerHour, err = envInt("MAGPIE_AUTH_VERIFY_FAILURES_PER_HOUR", 0); err != nil {
		return Config{}, err
	}
	if cfg.VerifyFailuresPerIPPerHour, err = envInt("MAGPIE_AUTH_VERIFY_FAILURES_PER_IP_PER_HOUR", 0); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envBool(name string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("auth: %s must be true or false", name)
	}
	return v, nil
}

func envInt(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer", ErrInvalidConfig, name)
	}
	return n, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration such as 10m", ErrInvalidConfig, name)
	}
	return d, nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withDefaults fills every unset policy value. It does not resolve
// AdminEmails or validate the result.
func (cfg Config) withDefaults() Config {
	if cfg.CodeTTL == 0 {
		cfg.CodeTTL = DefaultCodeTTL
	}
	if cfg.CodeMaxAttempts == 0 {
		cfg.CodeMaxAttempts = DefaultCodeMaxAttempts
	}
	if cfg.SendInterval == 0 {
		cfg.SendInterval = DefaultSendInterval
	}
	if cfg.SendsPerHour == 0 {
		cfg.SendsPerHour = DefaultSendsPerHour
	}
	if cfg.SendsPerIPPerHour == 0 {
		cfg.SendsPerIPPerHour = DefaultSendsPerIPPerHour
	}
	if cfg.VerifyFailuresPerHour == 0 {
		cfg.VerifyFailuresPerHour = DefaultVerifyFailuresPerHour
	}
	if cfg.VerifyFailuresPerIPPerHour == 0 {
		cfg.VerifyFailuresPerIPPerHour = DefaultVerifyFailuresPerIPPerHour
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.SessionTouchInterval == 0 {
		cfg.SessionTouchInterval = DefaultSessionTouchInterval
	}
	if cfg.CookieName == "" {
		cfg.CookieName = DefaultCookieName
	}
	if cfg.CookieSameSite == 0 {
		cfg.CookieSameSite = http.SameSiteLaxMode
	}
	return cfg
}

// Validate refuses a policy that would weaken sign-in. It never contacts the
// database or the network.
func (cfg Config) Validate() error {
	if len(cfg.Secret) < minSecretLength {
		return fmt.Errorf("%w: MAGPIE_AUTH_SECRET must be at least %d bytes", ErrInvalidConfig, minSecretLength)
	}
	if cfg.CodeTTL <= 0 {
		return fmt.Errorf("%w: CodeTTL must be positive", ErrInvalidConfig)
	}
	if cfg.SessionTTL <= 0 {
		return fmt.Errorf("%w: SessionTTL must be positive", ErrInvalidConfig)
	}
	if cfg.SessionTouchInterval < 0 {
		return fmt.Errorf("%w: SessionTouchInterval must not be negative", ErrInvalidConfig)
	}
	if cfg.CodeMaxAttempts <= 0 || cfg.CodeMaxAttempts > 20 {
		return fmt.Errorf("%w: CodeMaxAttempts must be between 1 and 20", ErrInvalidConfig)
	}
	if cfg.SendInterval < 0 || cfg.SendsPerHour <= 0 || cfg.SendsPerIPPerHour <= 0 {
		return fmt.Errorf("%w: send throttles must be non-negative and non-zero", ErrInvalidConfig)
	}
	if cfg.VerifyFailuresPerHour <= 0 || cfg.VerifyFailuresPerIPPerHour <= 0 {
		return fmt.Errorf("%w: verification throttles must be positive", ErrInvalidConfig)
	}
	switch cfg.CookieSameSite {
	case http.SameSiteDefaultMode, http.SameSiteLaxMode, http.SameSiteStrictMode:
	default:
		return fmt.Errorf("%w: CookieSameSite must be Lax or Strict", ErrInvalidConfig)
	}
	cookie := &http.Cookie{Name: cfg.CookieName, Value: "x", Domain: cfg.CookieDomain}
	if err := cookie.Valid(); err != nil {
		return fmt.Errorf("%w: cookie settings: %v", ErrInvalidConfig, err)
	}
	if strings.HasPrefix(cfg.CookieName, "__Host-") && (cfg.CookieDomain != "" || cfg.AllowInsecureCookie) {
		return fmt.Errorf("%w: a __Host- cookie needs no Domain and a Secure connection", ErrInvalidConfig)
	}
	if strings.HasPrefix(cfg.CookieName, "__Secure-") && cfg.AllowInsecureCookie {
		return fmt.Errorf("%w: a __Secure- cookie needs a Secure connection", ErrInvalidConfig)
	}
	if _, err := cfg.normalizedAdminEmails(); err != nil {
		return err
	}
	if _, err := cfg.trustedOriginSet(); err != nil {
		return err
	}
	return nil
}

func (cfg Config) normalizedAdminEmails() (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(cfg.AdminEmails))
	for _, raw := range cfg.AdminEmails {
		email, err := NormalizeEmail(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: admin email %q is not a usable address", ErrInvalidConfig, raw)
		}
		out[email] = struct{}{}
	}
	return out, nil
}

type origin struct {
	scheme string
	host   string
}

func (cfg Config) trustedOriginSet() (map[origin]struct{}, error) {
	out := make(map[origin]struct{}, len(cfg.TrustedOrigins))
	for _, raw := range cfg.TrustedOrigins {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%w: trusted origin %q must be scheme://host", ErrInvalidConfig, raw)
		}
		out[origin{scheme: u.Scheme, host: strings.ToLower(u.Host)}] = struct{}{}
	}
	return out, nil
}

// ErrInvalidConfig is returned by Config.Validate.
var ErrInvalidConfig = errors.New("auth: invalid configuration")
