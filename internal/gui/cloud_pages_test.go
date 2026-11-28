package gui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/auth"
	"github.com/yetone/magpie/internal/cloud/store"
)

type cloudPageMailer struct{}

func (cloudPageMailer) SendLoginCode(context.Context, auth.LoginCodeMessage) error { return nil }

type cloudSessionRepo struct {
	store.Store
	session store.Session
	user    store.User
}

func (r *cloudSessionRepo) SessionByTokenHash(context.Context, []byte, time.Time) (store.Session, store.User, error) {
	return r.session, r.user, nil
}

func cloudPageRuntime(t *testing.T, signedIn bool) *cloudRuntime {
	t.Helper()
	repo := &cloudSessionRepo{}
	if signedIn {
		verified := time.Now().UTC()
		repo.user = store.User{ID: uuid.New(), Email: "admin@example.com", EmailNormalized: "admin@example.com", Role: store.RoleAdmin, Status: store.StatusActive, AuthVersion: 1, EmailVerifiedAt: &verified}
		repo.session = store.Session{ID: uuid.New(), UserID: repo.user.ID, AuthVersion: 1, ExpiresAt: verified.Add(time.Hour)}
	}
	accounts, err := auth.New(auth.Config{Secret: bytes.Repeat([]byte{1}, 32)}, repo, cloudPageMailer{})
	if err != nil {
		t.Fatal(err)
	}
	return &cloudRuntime{auth: accounts}
}

func TestCloudPageRoutesAndHeaders(t *testing.T) {
	r := cloudPageRuntime(t, false)
	h := r.Handler()
	for _, path := range []string{"/", "/cloud/", "/login", "/register", "/reset"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Security-Policy") != cloudPageCSP {
			t.Errorf("%s: status %d cache %q CSP %q", path, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Security-Policy"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous /app: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCloudAuthPagesRedirectSignedInReader(t *testing.T) {
	r := cloudPageRuntime(t, true)
	h := r.Handler()
	for _, path := range []string{"/login", "/register", "/reset"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.DefaultCookieName, Value: "signed-in"})
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/app" {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/app", nil)
	req.AddCookie(&http.Cookie{Name: auth.DefaultCookieName, Value: "signed-in"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("signed-in /app: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
