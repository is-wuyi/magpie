package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/cloud/store"
)

func doJSON(t *testing.T, h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func decodeReply(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, w.Body.String())
	}
	return out
}

func TestServeCodeAnswerDoesNotRevealRegistration(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)

	first := doJSON(t, svc.ServeCode, http.MethodPost, RouteCode, `{"email":"new@example.com"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	if _, err := svc.repo.EnsureVerifiedUser(context.Background(), store.EnsureVerifiedUserParams{
		Email: "known@example.com", EmailNormalized: "known@example.com", VerifiedAt: testNowTime,
	}); err != nil {
		t.Fatal(err)
	}
	second := doJSON(t, svc.ServeCode, http.MethodPost, RouteCode, `{"email":"known@example.com"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second: %d %s", second.Code, second.Body)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("known and unknown addresses differ:\n%s\n%s", first.Body, second.Body)
	}
	for _, want := range []string{`"ok":true`, `"retryAfter":60`} {
		if !strings.Contains(first.Body.String(), want) {
			t.Errorf("body %s misses %s", first.Body, want)
		}
	}
	if strings.Contains(first.Body.String(), "new@example.com") {
		t.Fatal("the reply echoes the address")
	}
}

func TestServeLoginSetsCookieOnly(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "login@example.com")

	w := doJSON(t, svc.ServeLogin, http.MethodPost, RouteLogin, `{"email":"login@example.com","code":"`+code+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %+v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != DefaultCookieName || cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie: %+v", cookie)
	}
	body := w.Body.String()
	if strings.Contains(body, cookie.Value) {
		t.Fatal("the response body carries the session token")
	}
	reply := decodeReply(t, w)
	user, _ := reply["user"].(map[string]any)
	if user["email"] != "login@example.com" || user["role"] != "user" || user["emailVerified"] != true {
		t.Fatalf("user reply: %+v", reply)
	}
}

func TestServeLoginInvalidCodeIsGeneric(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "bad@example.com")
	w := doJSON(t, svc.ServeLogin, http.MethodPost, RouteLogin, `{"email":"bad@example.com","code":"`+wrongCode(code)+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	reply := decodeReply(t, w)
	if reply["error"] != "invalid_code" {
		t.Fatalf("reply: %+v", reply)
	}
	if strings.Contains(w.Body.String(), "bad@example.com") || strings.Contains(w.Body.String(), "fake database") {
		t.Fatalf("reply leaks detail: %s", w.Body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("a failed login set a cookie")
	}
}

func TestServeLogoutClearsCookieAndRevokes(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "logout@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "logout@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, RouteLogout, nil)
	r.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: login.Token})
	w := httptest.NewRecorder()
	svc.ServeLogout(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 || cookies[0].Value != "" {
		t.Fatalf("clear cookie: %+v", cookies)
	}
	if _, err := svc.Authenticate(context.Background(), login.Token); err != ErrUnauthenticated {
		t.Fatalf("token still live: %v", err)
	}
}

func TestServeMe(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "me@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "me@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, RouteMe, nil)
	r.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: login.Token})
	w := httptest.NewRecorder()
	svc.ServeMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	reply := decodeReply(t, w)
	user, _ := reply["user"].(map[string]any)
	if user["email"] != "me@example.com" || user["id"] != login.User.ID.String() {
		t.Fatalf("reply: %+v", reply)
	}

	w = httptest.NewRecorder()
	svc.ServeMe(w, httptest.NewRequest(http.MethodGet, RouteMe, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous me: %d %s", w.Code, w.Body)
	}

	r = httptest.NewRequest(http.MethodGet, RouteMe, nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{
		UserID: login.User.ID, Email: login.User.Email, Role: login.User.Role, AuthVersion: login.User.AuthVersion,
	}))
	w = httptest.NewRecorder()
	svc.ServeMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("context principal me: %d %s", w.Code, w.Body)
	}
}

func TestServeMeRejectsDisabledUser(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "gone@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "gone@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	u := repo.users[login.User.ID]
	u.Status = store.StatusDisabled
	repo.users[u.ID] = u
	repo.mu.Unlock()

	r := httptest.NewRequest(http.MethodGet, RouteMe, nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{UserID: login.User.ID, Email: login.User.Email, Role: login.User.Role}))
	w := httptest.NewRecorder()
	svc.ServeMe(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAuthWritesRejectAnotherOrigin(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	routes := []struct {
		h    http.HandlerFunc
		path string
		body string
	}{
		{svc.ServeCode, RouteCode, `{"email":"x@example.com"}`},
		{svc.ServeLogin, RouteLogin, `{"email":"x@example.com","code":"123456"}`},
		{svc.ServeLogout, RouteLogout, ``},
	}
	for _, route := range routes {
		for _, header := range []struct{ name, value string }{
			{"Sec-Fetch-Site", "cross-site"},
			{"Sec-Fetch-Site", "same-site"},
		} {
			r := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
			r.Host = "magpie.example"
			r.Header.Set(header.name, header.value)
			w := httptest.NewRecorder()
			route.h(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s with %s=%s: %d %s", route.path, header.name, header.value, w.Code, w.Body)
			}
		}
		r := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
		r.Host = "magpie.example"
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		route.h(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s with a foreign Origin: %d %s", route.path, w.Code, w.Body)
		}
		r = httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
		r.Host = "magpie.example"
		r.Header.Set("Origin", "https://magpie.example")
		w = httptest.NewRecorder()
		route.h(w, r)
		if w.Code == http.StatusForbidden {
			t.Errorf("%s with its own Origin was refused", route.path)
		}
	}
}

func TestAuthHandlerMethods(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	w := doJSON(t, svc.ServeCode, http.MethodGet, RouteCode, ``)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET code: %d %q", w.Code, w.Header().Get("Allow"))
	}
	w = doJSON(t, svc.ServeMe, http.MethodPost, RouteMe, ``)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST me: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestAuthRejectsUnknownAndOversizedBodies(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	w := doJSON(t, svc.ServeCode, http.MethodPost, RouteCode, `{"email":"a@example.com","admin":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body)
	}
	w = doJSON(t, svc.ServeCode, http.MethodPost, RouteCode, `{"email":"`+strings.Repeat("a", maxAuthBody)+`@example.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d %s", w.Code, w.Body)
	}
}

func TestRegisterRoutes(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	for _, path := range []string{RouteCode, RouteLogin, RouteLogout, RouteMe} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound {
			t.Errorf("%s is not registered", path)
		}
	}
}

func TestRequireUserAndAdmin(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	svc.RequireUser(ok).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous RequireUser: %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{Role: store.RoleUser}))
	w = httptest.NewRecorder()
	svc.RequireAdmin(ok).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("user RequireAdmin: %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{Role: store.RoleAdmin}))
	w = httptest.NewRecorder()
	svc.RequireAdmin(ok).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin RequireAdmin: %d", w.Code)
	}
}

func TestWithPrincipalMiddleware(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mustRequestCode(t, svc, mail, "mw@example.com")
	login, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "mw@example.com", Code: code})
	if err != nil {
		t.Fatal(err)
	}
	var seen *Principal
	h := svc.WithPrincipal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFromContext(r.Context()); ok {
			seen = &p
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: login.Token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if seen == nil || seen.UserID != login.User.ID {
		t.Fatalf("principal not attached: %+v", seen)
	}

	seen = nil
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if seen != nil {
		t.Fatal("anonymous request got a principal")
	}
}

func TestSessionCookieExpiresWithZeroClock(t *testing.T) {
	// The cookie's MaxAge comes from the service clock, not the wall clock,
	// so a login at a fixed time is reproducible.
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	svc.now = func() time.Time { return testNowTime.Add(2 * time.Hour) }
	c := svc.SessionCookie("t", testNowTime.Add(time.Hour))
	if c.MaxAge != 1 {
		t.Fatalf("an already-expired cookie got MaxAge %d", c.MaxAge)
	}
}
