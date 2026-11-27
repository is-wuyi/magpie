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

// PurposeLogin is the only purpose this package issues today. Codes are
// scoped by purpose in storage, so a code made for another purpose can never
// satisfy a login.
const PurposeLogin = DefaultLoginPurpose

// CodeRequest asks for a sign-in code. IP is the caller's address, used for
// the per-IP throttle; an empty value drops that dimension.
type CodeRequest struct {
	Email string
	IP    string
}

// CodeSent is the generic answer to a code request. It never says whether
// the address is already registered: every address can receive a code, and
// the same answer is returned either way.
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

// RequestLoginCode mails a fresh code. It checks the send throttle before
// storing the code and invalidates the previous code for the address. The
// code is never returned or logged.
func (s *Service) RequestLoginCode(ctx context.Context, req CodeRequest) (CodeSent, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return CodeSent{}, err
	}
	now := s.now()
	code, err := randomLoginCode()
	if err != nil {
		return CodeSent{}, fmt.Errorf("%w: generate code: %v", ErrUnavailable, err)
	}
	rec, err := s.repo.IssueEmailCode(ctx, store.IssueEmailCodeParams{
		EmailNormalized:  email,
		Purpose:          PurposeLogin,
		CodeHash:         s.hashLoginCode(PurposeLogin, email, code),
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
	msg := LoginCodeMessage{
		To:        email,
		Code:      code,
		Purpose:   PurposeLogin,
		ExpiresAt: rec.ExpiresAt,
	}
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

// VerifyLoginCode checks a code and creates a session. The user is created
// as an ordinary user on the first successful verification; Promotion to
// admin happens here, after the address was proven, and only for an address
// in Config.AdminEmails.
func (s *Service) VerifyLoginCode(ctx context.Context, req LoginRequest) (LoginResult, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return LoginResult{}, err
	}
	code := strings.TrimSpace(req.Code)
	if !isSixDigits(code) {
		return LoginResult{}, ErrInvalidCode
	}
	now := s.now()
	ip := canonicalIP(req.IP)

	counts, err := s.repo.EmailCodeFailureCounts(ctx, email, ip, now.Add(-throttleWindow))
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if s.cfg.VerifyFailuresPerHour > 0 && counts.ByEmail >= s.cfg.VerifyFailuresPerHour {
		return LoginResult{}, &RateLimitError{RetryAfter: throttleWindow}
	}
	if ip != "" && s.cfg.VerifyFailuresPerIPPerHour > 0 && counts.ByIP >= s.cfg.VerifyFailuresPerIPPerHour {
		return LoginResult{}, &RateLimitError{RetryAfter: throttleWindow}
	}

	rec, err := s.repo.LatestActiveEmailCode(ctx, email, PurposeLogin, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return LoginResult{}, ErrInvalidCode
		}
		return LoginResult{}, mapStoreError(err)
	}
	expected := s.hashLoginCode(PurposeLogin, email, code)
	if !hmac.Equal(expected, rec.CodeHash) {
		if _, err := s.repo.RecordEmailCodeFailure(ctx, rec.ID, now); err != nil && !errors.Is(err, store.ErrCodeUnavailable) {
			return LoginResult{}, mapStoreError(err)
		}
		return LoginResult{}, ErrInvalidCode
	}
	if err := s.repo.ConsumeEmailCode(ctx, rec.ID, expected, now); err != nil {
		if errors.Is(err, store.ErrCodeUnavailable) {
			return LoginResult{}, ErrInvalidCode
		}
		return LoginResult{}, mapStoreError(err)
	}

	user, err := s.repo.EnsureVerifiedUser(ctx, store.EnsureVerifiedUserParams{
		Email:           email,
		EmailNormalized: email,
		VerifiedAt:      now,
		PromoteAdmin:    s.isAdmin(email),
	})
	if err != nil {
		if errors.Is(err, store.ErrUserDisabled) {
			return LoginResult{}, ErrAccountDisabled
		}
		return LoginResult{}, mapStoreError(err)
	}

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
		UserAgent:   req.UserAgent,
		RequestIP:   ip,
	})
	if err != nil {
		if errors.Is(err, store.ErrSessionStale) {
			return LoginResult{}, ErrSignInFailed
		}
		return LoginResult{}, mapStoreError(err)
	}
	return LoginResult{
		User:      user,
		Session:   session,
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil
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
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnauthenticated
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
