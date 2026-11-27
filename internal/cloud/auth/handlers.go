package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/cloud/store"
)

const (
	maxAuthBody        = 8 << 10
	maxUserAgentLength = 512
)

// Auth routes. They are not registered by this package anywhere: the caller
// mounts them, so the GUI can decide when the cloud sign-in pages exist.
const (
	RouteCode   = "/api/auth/code"
	RouteLogin  = "/api/auth/login"
	RouteLogout = "/api/auth/logout"
	RouteMe     = "/api/auth/me"
)

// RegisterRoutes adds the four auth routes to mux. Nothing calls this
// package's handlers until a caller does, which keeps the local app's
// existing routes unchanged.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+RouteCode, s.ServeCode)
	mux.HandleFunc("POST "+RouteLogin, s.ServeLogin)
	mux.HandleFunc("POST "+RouteLogout, s.ServeLogout)
	mux.HandleFunc("GET "+RouteMe, s.ServeMe)
}

// Handler returns a mux holding only the auth routes, for a deployment that
// wants to mount them as one unit.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	return mux
}

type codeBody struct {
	Email string `json:"email"`
}

type codeReply struct {
	OK         bool `json:"ok"`
	RetryAfter int  `json:"retryAfter"`
}

type loginBody struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type userReply struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Role          string    `json:"role"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
}

type loginReply struct {
	User      userReply `json:"user"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type meReply struct {
	User userReply `json:"user"`
}

type errorReply struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retryAfter,omitempty"`
}

// ServeCode handles POST /api/auth/code. Its answer is the same whether the
// address is new or known: every address can receive a code.
func (s *Service) ServeCode(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.sameOriginWrite(r) {
		writeAuthError(w, r, ErrForbidden)
		return
	}
	var body codeBody
	if err := readAuthJSON(w, r, &body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, errorReply{Error: "invalid_request", Message: "The request is not valid."})
		return
	}
	sent, err := s.RequestLoginCode(r.Context(), CodeRequest{
		Email: body.Email,
		IP:    ClientIP(r, s.cfg.TrustProxyHeaders),
	})
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	writeJSONStatus(w, http.StatusOK, codeReply{OK: true, RetryAfter: seconds(sent.RetryAfter)})
}

// ServeLogin handles POST /api/auth/login and sets the session cookie on
// success. The token is in the cookie only.
func (s *Service) ServeLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.sameOriginWrite(r) {
		writeAuthError(w, r, ErrForbidden)
		return
	}
	var body loginBody
	if err := readAuthJSON(w, r, &body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, errorReply{Error: "invalid_request", Message: "The request is not valid."})
		return
	}
	result, err := s.VerifyLoginCode(r.Context(), LoginRequest{
		Email:     body.Email,
		Code:      body.Code,
		IP:        ClientIP(r, s.cfg.TrustProxyHeaders),
		UserAgent: truncate(strings.ToValidUTF8(r.UserAgent(), ""), maxUserAgentLength),
	})
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	s.SetSessionCookie(w, result.Token, result.ExpiresAt)
	writeJSONStatus(w, http.StatusOK, loginReply{
		User:      replyUser(result.User),
		ExpiresAt: result.ExpiresAt.UTC(),
	})
}

// ServeLogout handles POST /api/auth/logout. The cookie is cleared even when
// revoking the server row fails, so the browser does not keep presenting a
// token the server could not revoke.
func (s *Service) ServeLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.sameOriginWrite(r) {
		writeAuthError(w, r, ErrForbidden)
		return
	}
	token, _ := s.TokenFromRequest(r)
	s.SetClearSessionCookie(w)
	if err := s.Logout(r.Context(), token); err != nil {
		writeAuthError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeMe handles GET /api/auth/me. It works after WithPrincipal and also on
// its own, since it authenticates the cookie itself.
func (s *Service) ServeMe(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		var err error
		p, err = s.AuthenticateRequest(r)
		if err != nil {
			writeAuthError(w, r, err)
			return
		}
	}
	user, err := s.repo.UserByID(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAuthError(w, r, ErrUnauthenticated)
			return
		}
		writeAuthError(w, r, mapStoreError(err))
		return
	}
	if user.Status != store.StatusActive || user.EmailVerifiedAt == nil {
		writeAuthError(w, r, ErrUnauthenticated)
		return
	}
	writeJSONStatus(w, http.StatusOK, meReply{User: replyUser(user)})
}

func replyUser(u store.User) userReply {
	return userReply{
		ID:            u.ID.String(),
		Email:         u.Email,
		Role:          string(u.Role),
		EmailVerified: u.EmailVerifiedAt != nil,
		CreatedAt:     u.CreatedAt.UTC(),
	}
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAuthError maps a service error to a status and a message that says
// nothing about whether an address exists, which code failed, or what the
// database said.
func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	var rl *RateLimitError
	switch {
	case errors.As(err, &rl):
		wait := seconds(rl.RetryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeJSONStatus(w, http.StatusTooManyRequests, errorReply{
			Error: "rate_limited", Message: "Too many requests. Try again later.", RetryAfter: wait,
		})
	case errors.Is(err, ErrInvalidEmail):
		writeJSONStatus(w, http.StatusBadRequest, errorReply{Error: "invalid_email", Message: "Enter a valid email address."})
	case errors.Is(err, ErrInvalidCode):
		writeJSONStatus(w, http.StatusUnauthorized, errorReply{Error: "invalid_code", Message: "That code is not valid or has expired."})
	case errors.Is(err, ErrTooManyRequests):
		writeJSONStatus(w, http.StatusTooManyRequests, errorReply{Error: "rate_limited", Message: "Too many requests. Try again later."})
	case errors.Is(err, ErrAccountDisabled):
		writeJSONStatus(w, http.StatusForbidden, errorReply{Error: "account_disabled", Message: "This account cannot sign in."})
	case errors.Is(err, ErrSignInFailed):
		writeJSONStatus(w, http.StatusConflict, errorReply{Error: "sign_in_failed", Message: "Sign-in did not complete. Request a new code."})
	case errors.Is(err, ErrUnauthenticated):
		writeJSONStatus(w, http.StatusUnauthorized, errorReply{Error: "unauthenticated", Message: "Sign in to continue."})
	case errors.Is(err, ErrForbidden):
		writeJSONStatus(w, http.StatusForbidden, errorReply{Error: "forbidden", Message: "This request is not allowed."})
	case errors.Is(err, ErrMailFailed):
		writeJSONStatus(w, http.StatusInternalServerError, errorReply{Error: "mail_failed", Message: "The code could not be sent. Try again later."})
	default:
		writeJSONStatus(w, http.StatusInternalServerError, errorReply{Error: "unavailable", Message: "Something went wrong. Try again later."})
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSONStatus(w, http.StatusMethodNotAllowed, errorReply{Error: "method_not_allowed", Message: "This method is not allowed."})
}

func readAuthJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("auth: request body has more than one JSON value")
	}
	return nil
}

func seconds(d time.Duration) int {
	s := int(d / time.Second)
	if d > 0 && s == 0 {
		return 1
	}
	if s < 0 {
		return 0
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
