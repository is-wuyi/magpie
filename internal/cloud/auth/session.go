package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/cloud/store"
)

// Principal is who a verified session belongs to. It is put in a request's
// context by WithPrincipal and never read from a header or a URL.
type Principal struct {
	UserID      uuid.UUID
	SessionID   uuid.UUID
	Email       string
	Role        store.Role
	AuthVersion int
	ExpiresAt   time.Time
}

// IsAdmin reports whether the principal may use an administrative route.
func (p Principal) IsAdmin() bool { return p.Role == store.RoleAdmin }

type principalContextKey struct{}

// WithPrincipal returns a context carrying p. It is exported so a test or a
// server-side job can build the same context WithPrincipal makes.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the principal WithPrincipal stored, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

// SessionCookie builds the cookie that carries token. It is HttpOnly,
// Secure unless Config.AllowInsecureCookie says otherwise, SameSite=Lax (or
// Strict), and scoped to the whole site so both the page and its API see it.
func (s *Service) SessionCookie(token string, expires time.Time) *http.Cookie {
	maxAge := int(expires.Sub(s.now()).Seconds())
	if maxAge <= 0 {
		maxAge = 1
	}
	return &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    token,
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		Expires:  expires.UTC(),
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !s.cfg.AllowInsecureCookie,
		SameSite: s.cfg.CookieSameSite,
	}
}

// ClearSessionCookie builds the cookie that removes a session cookie,
// keeping the same Path, Domain and security attributes so browsers match
// it.
func (s *Service) ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !s.cfg.AllowInsecureCookie,
		SameSite: s.cfg.CookieSameSite,
	}
}

// SetSessionCookie writes a session cookie.
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, s.SessionCookie(token, expires))
}

// SetClearSessionCookie removes a session cookie.
func (s *Service) SetClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, s.ClearSessionCookie())
}

// CookieName is the configured name of the session cookie.
func (s *Service) CookieName() string { return s.cfg.CookieName }

// TokenFromRequest returns the session token from the request's cookie. It
// does not accept a token from a query string or a body.
func (s *Service) TokenFromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	c, err := r.Cookie(s.cfg.CookieName)
	if err != nil || c.Value == "" || len(c.Value) > maxTokenLength {
		return "", false
	}
	return c.Value, true
}

// Authenticate resolves a token to a principal. A revoked, expired or
// disabled session is ErrUnauthenticated; a storage failure is
// ErrUnavailable, so a caller never mistakes a failed read for a live
// session.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" || len(token) > maxTokenLength {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now()
	session, user, err := s.repo.SessionByTokenHash(ctx, s.hashSessionToken(token), now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, mapStoreError(err)
	}
	// last_seen_at is maintenance, not proof: a failed touch does not
	// invalidate a session that the lookup just proved live. Skip the round
	// trip until the configured interval has passed.
	if s.cfg.SessionTouchInterval <= 0 || !session.LastSeenAt.Add(s.cfg.SessionTouchInterval).After(now) {
		_ = s.repo.TouchSession(ctx, session.ID, now, s.cfg.SessionTouchInterval)
	}
	return Principal{
		UserID:      user.ID,
		SessionID:   session.ID,
		Email:       user.Email,
		Role:        user.Role,
		AuthVersion: user.AuthVersion,
		ExpiresAt:   session.ExpiresAt,
	}, nil
}

// AuthenticateRequest authenticates the cookie on r.
func (s *Service) AuthenticateRequest(r *http.Request) (Principal, error) {
	token, ok := s.TokenFromRequest(r)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return s.Authenticate(r.Context(), token)
}

// Logout revokes the session the token names. It is idempotent: a token
// that is already gone is not an error, so a repeated logout works.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" || len(token) > maxTokenLength {
		return nil
	}
	err := s.repo.RevokeSessionByTokenHash(ctx, s.hashSessionToken(token), s.now())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return mapStoreError(err)
	}
	return nil
}

// LogoutAll revokes every session of a user and raises their auth version,
// so any session this process did not see stops working too.
func (s *Service) LogoutAll(ctx context.Context, p Principal) error {
	if p.UserID == uuid.Nil {
		return ErrUnauthenticated
	}
	_, err := s.repo.RevokeUserSessions(ctx, p.UserID, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnauthenticated
		}
		return mapStoreError(err)
	}
	return nil
}

// WithPrincipal attaches the principal of a live session cookie to the
// request context. A missing, invalid or expired cookie continues as an
// anonymous request; RequireUser or RequireAdmin is what turns that away.
func (s *Service) WithPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := s.TokenFromRequest(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.Authenticate(r.Context(), token)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireUser turns away a request with no principal. Mount it after
// WithPrincipal.
func (s *Service) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); !ok {
			writeAuthError(w, r, ErrUnauthenticated)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin turns away a request with no principal or a non-admin one.
// Mount it after WithPrincipal.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok {
			writeAuthError(w, r, ErrUnauthenticated)
			return
		}
		if !p.IsAdmin() {
			writeAuthError(w, r, ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the request's address for throttling. With trustProxy it
// reads the first valid X-Forwarded-For entry, then X-Real-IP; a proxy that
// does not overwrite those headers must not be trusted. A header that is
// present but unusable yields "" rather than the proxy's address, so one
// bad proxy cannot make every user share one throttle bucket.
func ClientIP(r *http.Request, trustProxy bool) string {
	if r == nil {
		return ""
	}
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			for _, part := range strings.Split(xff, ",") {
				if ip := canonicalIP(strings.TrimSpace(strings.Trim(part, `"`))); ip != "" {
					return ip
				}
			}
			return ""
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return canonicalIP(strings.TrimSpace(strings.Trim(xr, `"`)))
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return canonicalIP(host)
	}
	return canonicalIP(r.RemoteAddr)
}
