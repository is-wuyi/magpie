package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/cloud/store"
)

const (
	// codeHashVersion is stored with each code so a future hash change can
	// be rolled out without guessing what an old row used.
	codeHashVersion int16 = 1

	throttleWindow = time.Hour
	maxTokenLength = 256
)

const (
	PurposeLogin    = DefaultLoginPurpose
	PurposeRegister = "register"
	PurposeReset    = "reset"
)

// CodeRequest asks for a sign-in code. IP is the caller's address, used for
// the per-IP throttle; an empty value drops that dimension.
type CodeRequest struct {
	Email string
	IP    string
}

// CodeSent is the generic answer to a code request. It never says whether the
// address is already registered: every address can receive a code, and the
// same answer is returned either way.
type CodeSent struct {
	RetryAfter time.Duration
}

// LoginRequest verifies a code and, on success, signs the address in.
type LoginRequest struct {
	Email     string
	Code      string
	IP        string
	UserAgent string
}

// LoginResult is a completed sign-in. Token is returned once and must go
// into the session cookie, never into a body or a log.
type LoginResult struct {
	User      store.User
	Session   store.Session
	Token     string
	ExpiresAt time.Time
}

// PublicPolicy is the safe part of the account policy a page needs to choose
// its forms. Secrets, mail configuration and administrator addresses are not
// included.
type PublicPolicy struct {
	RegistrationEnabled  bool `json:"registrationEnabled"`
	PasswordLoginEnabled bool `json:"passwordLoginEnabled"`
	CodeLoginEnabled     bool `json:"codeLoginEnabled"`
	PasswordMinLength    int  `json:"passwordMinLength"`
}

// PublicAuthPolicy returns the current policy for the landing and auth pages.
func (s *Service) PublicAuthPolicy(ctx context.Context) (PublicPolicy, error) {
	p, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return PublicPolicy{}, mapStoreError(err)
	}
	return PublicPolicy{
		RegistrationEnabled:  p.RegistrationEnabled,
		PasswordLoginEnabled: p.PasswordLoginEnabled,
		CodeLoginEnabled:     p.CodeLoginEnabled,
		PasswordMinLength:    p.PasswordMinLength,
	}, nil
}

// RequestLoginCode mails a fresh code to an existing account. Registration is
// deliberately a separate purpose and endpoint.
func (s *Service) RequestLoginCode(ctx context.Context, req CodeRequest) (CodeSent, error) {
	return s.requestCode(ctx, req, PurposeLogin)
}

func (s *Service) requestCode(ctx context.Context, req CodeRequest, purpose string) (CodeSent, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return CodeSent{}, err
	}
	policy, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return CodeSent{}, mapStoreError(err)
	}
	switch purpose {
	case PurposeLogin:
		if !policy.CodeLoginEnabled {
			return CodeSent{}, ErrCodeLoginDisabled
		}
	case PurposeRegister:
		if !policy.RegistrationEnabled {
			return CodeSent{}, ErrRegistrationDisabled
		}
	case PurposeReset:
		if !policy.PasswordLoginEnabled {
			return CodeSent{}, ErrPasswordLoginDisabled
		}
	default:
		return CodeSent{}, fmt.Errorf("%w: unknown code purpose", ErrUnavailable)
	}
	now := s.now()
	code, err := randomLoginCode()
	if err != nil {
		return CodeSent{}, fmt.Errorf("%w: generate code: %v", ErrUnavailable, err)
	}
	rec, err := s.repo.IssueEmailCode(ctx, store.IssueEmailCodeParams{
		EmailNormalized:  email,
		Purpose:          purpose,
		CodeHash:         s.hashLoginCode(purpose, email, code),
		HashVersion:      codeHashVersion,
		MaxAttempts:      int16(s.cfg.CodeMaxAttempts),
		Now:              now,
		ExpiresAt:        now.Add(s.cfg.CodeTTL),
		RequestIP:        canonicalIP(req.IP),
		Window:           throttleWindow,
		SendInterval:     s.cfg.SendInterval,
		MaxSendsPerEmail: s.cfg.SendsPerHour,
		MaxSendsPerIP:    s.cfg.SendsPerIPPerHour,
	})
	if err != nil {
		return CodeSent{}, mapStoreError(err)
	}
	msg := LoginCodeMessage{To: email, Code: code, Purpose: purpose, ExpiresAt: rec.ExpiresAt}
	if err := s.mailer.SendLoginCode(ctx, msg); err != nil {
		// A code nobody received must not stay usable. The delete is best
		// effort: a storage failure here is reported with the mail failure,
		// and the row still expires on its own.
		delErr := s.repo.DeleteEmailCode(ctx, rec.ID)
		if delErr != nil {
			return CodeSent{}, fmt.Errorf("%w: %w", ErrMailFailed, errors.Join(err, delErr))
		}
		return CodeSent{}, fmt.Errorf("%w: %w", ErrMailFailed, err)
	}
	return CodeSent{RetryAfter: s.cfg.SendInterval}, nil
}

// VerifyLoginCode checks a code and signs in an existing account. It never
// creates an account; registration has its own purpose and endpoint.
func (s *Service) VerifyLoginCode(ctx context.Context, req LoginRequest) (LoginResult, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return LoginResult{}, err
	}
	policy, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if !policy.CodeLoginEnabled {
		return LoginResult{}, ErrCodeLoginDisabled
	}
	if _, err := s.verifyEmailCode(ctx, email, PurposeLogin, req.Code, req.IP); err != nil {
		return LoginResult{}, err
	}
	credentials, err := s.repo.CredentialsByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return LoginResult{}, ErrInvalidCode
	}
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if credentials.User.Status != store.StatusActive {
		return LoginResult{}, ErrAccountDisabled
	}
	return s.completeLogin(ctx, credentials.User, req)
}

// verifyEmailCode consumes a code only after its purpose, address and digest
// all match. Failure throttles are shared across all three purposes.
func (s *Service) verifyEmailCode(ctx context.Context, email, purpose, rawCode, requestIP string) (store.EmailCode, error) {
	rec, expected, err := s.checkEmailCode(ctx, email, purpose, rawCode, requestIP)
	if err != nil {
		return store.EmailCode{}, err
	}
	if err := s.repo.ConsumeEmailCode(ctx, rec.ID, expected, s.now()); err != nil {
		if errors.Is(err, store.ErrCodeUnavailable) {
			return store.EmailCode{}, ErrInvalidCode
		}
		return store.EmailCode{}, mapStoreError(err)
	}
	return rec, nil
}

// checkEmailCode verifies without consuming. Registration and reset consume
// in the same database transaction as the account change, not before it.
func (s *Service) checkEmailCode(ctx context.Context, email, purpose, rawCode, requestIP string) (store.EmailCode, []byte, error) {
	code := strings.TrimSpace(rawCode)
	if !isSixDigits(code) {
		return store.EmailCode{}, nil, ErrInvalidCode
	}
	now := s.now()
	ip := canonicalIP(requestIP)
	counts, err := s.repo.EmailCodeFailureCounts(ctx, email, ip, now.Add(-throttleWindow))
	if err != nil {
		return store.EmailCode{}, nil, mapStoreError(err)
	}
	if s.cfg.VerifyFailuresPerHour > 0 && counts.ByEmail >= s.cfg.VerifyFailuresPerHour {
		return store.EmailCode{}, nil, &RateLimitError{RetryAfter: throttleWindow}
	}
	if ip != "" && s.cfg.VerifyFailuresPerIPPerHour > 0 && counts.ByIP >= s.cfg.VerifyFailuresPerIPPerHour {
		return store.EmailCode{}, nil, &RateLimitError{RetryAfter: throttleWindow}
	}
	rec, err := s.repo.LatestActiveEmailCode(ctx, email, purpose, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EmailCode{}, nil, ErrInvalidCode
		}
		return store.EmailCode{}, nil, mapStoreError(err)
	}
	expected := s.hashLoginCode(purpose, email, code)
	if !hmac.Equal(expected, rec.CodeHash) {
		if _, err := s.repo.RecordEmailCodeFailure(ctx, rec.ID, now); err != nil && !errors.Is(err, store.ErrCodeUnavailable) {
			return store.EmailCode{}, nil, mapStoreError(err)
		}
		return store.EmailCode{}, nil, ErrInvalidCode
	}
	return rec, expected, nil
}

// completeLogin is the common session-issuing tail of every authentication
// method. It always creates a fresh random token after the method has proved
// the account, never before.
func (s *Service) completeLogin(ctx context.Context, user store.User, req LoginRequest) (LoginResult, error) {
	if user.Status != store.StatusActive || user.EmailVerifiedAt == nil {
		return LoginResult{}, ErrSignInFailed
	}
	now := s.now()
	token, err := randomSessionToken()
	if err != nil {
		return LoginResult{}, fmt.Errorf("%w: generate session: %v", ErrUnavailable, err)
	}
	expiresAt := now.Add(s.cfg.SessionTTL)
	session, err := s.repo.CreateSession(ctx, store.CreateSessionParams{
		UserID:      user.ID,
		TokenHash:   s.hashSessionToken(token),
		AuthVersion: user.AuthVersion,
		Now:         now,
		ExpiresAt:   expiresAt,
		UserAgent:   truncate(strings.ToValidUTF8(req.UserAgent, ""), maxUserAgentLength),
		RequestIP:   canonicalIP(req.IP),
	})
	if err != nil {
		if errors.Is(err, store.ErrSessionStale) {
			return LoginResult{}, ErrSignInFailed
		}
		return LoginResult{}, mapStoreError(err)
	}
	return LoginResult{User: user, Session: session, Token: token, ExpiresAt: expiresAt}, nil
}

// hashLoginCode binds the code to its purpose and address, so a digest
// copied to another row cannot be replayed there.
func (s *Service) hashLoginCode(purpose, email, code string) []byte {
	mac := hmac.New(sha256.New, s.codeKey)
	_, _ = mac.Write([]byte("magpie/auth/code/v1"))
	for _, part := range []string{purpose, email, code} {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(part))
	}
	return mac.Sum(nil)
}

func (s *Service) hashSessionToken(token string) []byte {
	mac := hmac.New(sha256.New, s.sessionKey)
	_, _ = mac.Write([]byte("magpie/auth/session/v1\x00"))
	_, _ = mac.Write([]byte(token))
	return mac.Sum(nil)
}

// randomLoginCode draws six decimal digits from crypto/rand without the
// modulo bias a byte % 10 would have.
func randomLoginCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func randomSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalIP keeps a server-derived address in one spelling. An address
// that cannot be parsed becomes empty so it adds no throttle dimension; it
// is never guessed.
func canonicalIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(raw); err == nil && addr.Zone() == "" {
		return addr.Unmap().String()
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil && addr.Zone() == "" {
			return addr.Unmap().String()
		}
	}
	return ""
}

func mapStoreError(err error) error {
	var rl *store.RateLimitError
	if errors.As(err, &rl) {
		return &RateLimitError{RetryAfter: rl.RetryAfter}
	}
	if errors.Is(err, store.ErrRateLimited) {
		return &RateLimitError{RetryAfter: time.Minute}
	}
	if errors.Is(err, store.ErrUserDisabled) {
		return ErrAccountDisabled
	}
	if errors.Is(err, store.ErrEmailTaken) {
		return ErrEmailTaken
	}
	if errors.Is(err, store.ErrPolicyConflict) {
		return ErrPolicyConflict
	}
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnauthenticated
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
