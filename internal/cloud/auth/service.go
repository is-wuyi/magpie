package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/cloud/store"
)

// Repository is the storage the service needs. internal/cloud/store's
// Store implements it; tests use fakes so the rules run without PostgreSQL.
type Repository interface {
	IssueEmailCode(ctx context.Context, p store.IssueEmailCodeParams) (store.EmailCode, error)
	LatestActiveEmailCode(ctx context.Context, emailNormalized, purpose string, now time.Time) (store.EmailCode, error)
	RecordEmailCodeFailure(ctx context.Context, id uuid.UUID, now time.Time) (store.EmailCodeAttempt, error)
	ConsumeEmailCode(ctx context.Context, id uuid.UUID, codeHash []byte, now time.Time) error
	DeleteEmailCode(ctx context.Context, id uuid.UUID) error
	EmailCodeFailureCounts(ctx context.Context, emailNormalized, requestIP string, since time.Time) (store.FailureCounts, error)
	EnsureVerifiedUser(ctx context.Context, p store.EnsureVerifiedUserParams) (store.User, error)
	UserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	CreateSession(ctx context.Context, p store.CreateSessionParams) (store.Session, error)
	SessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (store.Session, store.User, error)
	TouchSession(ctx context.Context, id uuid.UUID, now time.Time, minInterval time.Duration) error
	RevokeSessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) error
	RevokeUserSessions(ctx context.Context, userID uuid.UUID, now time.Time) (int, error)
}

var _ Repository = (*store.Store)(nil)

// Service is the sign-in policy. It is safe for concurrent use once built;
// it holds no mutable state of its own.
type Service struct {
	repo           Repository
	mailer         Mailer
	cfg            Config
	adminEmails    map[string]struct{}
	trustedOrigins map[origin]struct{}
	codeKey        []byte
	sessionKey     []byte
	now            func() time.Time
}

// New validates the policy and keys the code and session hashes. It fails
// rather than starting with a weak secret, an unusable throttle, a
// malformed admin address or no mailer.
func New(cfg Config, repo Repository, mailer Mailer) (*Service, error) {
	if repo == nil {
		return nil, fmt.Errorf("auth: a repository is required")
	}
	if mailer == nil {
		return nil, fmt.Errorf("auth: a mailer is required")
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	admins, err := cfg.normalizedAdminEmails()
	if err != nil {
		return nil, err
	}
	origins, err := cfg.trustedOriginSet()
	if err != nil {
		return nil, err
	}
	return &Service{
		repo:           repo,
		mailer:         mailer,
		cfg:            cfg,
		adminEmails:    admins,
		trustedOrigins: origins,
		codeKey:        deriveKey(cfg.Secret, "magpie/auth/code/v1"),
		sessionKey:     deriveKey(cfg.Secret, "magpie/auth/session/v1"),
		now:            func() time.Time { return time.Now().UTC() },
	}, nil
}

// deriveKey domain-separates the one deployment secret into keys that
// cannot be used in each other's place.
func deriveKey(secret []byte, label string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(label))
	return mac.Sum(nil)
}

func (s *Service) isAdmin(emailNormalized string) bool {
	_, ok := s.adminEmails[emailNormalized]
	return ok
}

// sameOriginWrite reports whether a browser write may proceed. A browser's
// Sec-Fetch-Site is authoritative; an older browser's Origin is compared
// with the request's Host and the configured trusted origins. A request
// with neither header (a non-browser client) is allowed; the cookie itself
// is SameSite=Lax, so a cross-site form post carries no session.
func (s *Service) sameOriginWrite(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	if raw == "null" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	_, ok := s.trustedOrigins[origin{scheme: u.Scheme, host: strings.ToLower(u.Host)}]
	return ok
}
