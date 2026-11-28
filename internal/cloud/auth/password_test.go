package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/cloud/store"
)

func TestRegisterPasswordLoginAndReset(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, func(cfg *Config) { cfg.AdminEmails = []string{"admin@example.com"} })
	email := "admin@example.com"
	if _, err := svc.RequestRegisterCode(context.Background(), CodeRequest{Email: email, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	code := mail.last().Code
	if mail.last().Purpose != PurposeRegister {
		t.Fatalf("purpose %q", mail.last().Purpose)
	}
	registered, err := svc.Register(context.Background(), RegisterRequest{Email: email, Code: code, Password: "initial-password", IP: "203.0.113.9", UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if registered.User.Role != store.RoleAdmin || registered.User.EmailVerifiedAt == nil {
		t.Fatalf("registered user: %+v", registered.User)
	}
	credentials, err := repo.CredentialsByEmail(context.Background(), email)
	if err != nil || credentials.PasswordHash == "" || credentials.PasswordHash == "initial-password" || credentials.PasswordHashVersion != passwordHashVersion {
		t.Fatalf("credentials: %+v, %v", credentials, err)
	}
	if _, err := svc.PasswordLogin(context.Background(), PasswordLoginRequest{Email: email, Password: "wrong-password", IP: "203.0.113.9"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := svc.PasswordLogin(context.Background(), PasswordLoginRequest{Email: email, Password: "initial-password", IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestResetCode(context.Background(), CodeRequest{Email: email, IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	resetCode := mail.last().Code
	if mail.last().Purpose != PurposeReset {
		t.Fatalf("reset purpose %q", mail.last().Purpose)
	}
	if _, err := svc.ResetPassword(context.Background(), ResetPasswordRequest{Email: email, Code: resetCode, Password: "replacement-password", IP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), registered.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("pre-reset session still works: %v", err)
	}
	if _, err := svc.PasswordLogin(context.Background(), PasswordLoginRequest{Email: email, Password: "initial-password"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := svc.PasswordLogin(context.Background(), PasswordLoginRequest{Email: email, Password: "replacement-password"}); err != nil {
		t.Fatalf("new password: %v", err)
	}
}

func TestRegistrationIsAtomicAndCodePurposesAreIsolated(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	email := "new@example.com"
	if _, err := svc.RequestRegisterCode(context.Background(), CodeRequest{Email: email}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.Register(context.Background(), RegisterRequest{Email: email, Code: mail.last().Code, Password: "initial-password"})
	if err != nil {
		t.Fatal(err)
	}
	if first.User.EmailNormalized != email {
		t.Fatalf("registered %+v", first.User)
	}
	if _, err := svc.RequestRegisterCode(context.Background(), CodeRequest{Email: email}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register(context.Background(), RegisterRequest{Email: email, Code: mail.last().Code, Password: "another-password"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate registration: %v", err)
	}
	loginCode := mustRequestCode(t, svc, mail, email)
	if _, err := svc.Register(context.Background(), RegisterRequest{Email: email, Code: loginCode, Password: "another-password"}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("login code accepted for registration: %v", err)
	}
	if _, err := svc.RequestRegisterCode(context.Background(), CodeRequest{Email: "other@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResetPassword(context.Background(), ResetPasswordRequest{Email: "other@example.com", Code: mail.last().Code, Password: "replacement-password"}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("registration code accepted for reset: %v", err)
	}
}

func TestCodeLoginDoesNotRegisterAnUnknownAddress(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	code := mailCode(t, svc, mail, "unknown@example.com")
	if _, err := svc.VerifyLoginCode(context.Background(), LoginRequest{Email: "unknown@example.com", Code: code}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("unknown address signed in: %v", err)
	}
	if repo.userCount() != 0 {
		t.Fatal("code login created an account")
	}
}

func mailCode(t *testing.T, svc *Service, mail *fakeMailer, email string) string {
	t.Helper()
	if _, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: email}); err != nil {
		t.Fatal(err)
	}
	return mail.last().Code
}

func TestAuthenticationPolicyDisablesEachFlow(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	repo.policy.RegistrationEnabled = false
	if _, err := svc.RequestRegisterCode(context.Background(), CodeRequest{Email: "new@example.com"}); !errors.Is(err, ErrRegistrationDisabled) {
		t.Fatalf("registration policy: %v", err)
	}
	repo.policy.PasswordLoginEnabled = false
	if _, err := svc.PasswordLogin(context.Background(), PasswordLoginRequest{Email: "new@example.com", Password: "initial-password"}); !errors.Is(err, ErrPasswordLoginDisabled) {
		t.Fatalf("password policy: %v", err)
	}
	if _, err := svc.RequestResetCode(context.Background(), CodeRequest{Email: "new@example.com"}); !errors.Is(err, ErrPasswordLoginDisabled) {
		t.Fatalf("reset policy: %v", err)
	}
	repo.policy.CodeLoginEnabled = false
	if _, err := svc.RequestLoginCode(context.Background(), CodeRequest{Email: "new@example.com"}); !errors.Is(err, ErrCodeLoginDisabled) {
		t.Fatalf("code-login policy: %v", err)
	}
}

func TestPasswordLengthBoundaries(t *testing.T) {
	for _, password := range []string{"short", strings.Repeat("x", 73)} {
		if _, err := hashPassword(password, 12); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("length %d: %v", len(password), err)
		}
	}
	if hash, err := hashPassword("exactly-12-chars", 12); err != nil || len(hash) == 0 {
		t.Fatalf("valid password: %v", err)
	}
}

func TestRegisterPasswordAndResetHandlers(t *testing.T) {
	repo, mail := newFakeRepo(), &fakeMailer{}
	svc := newTestService(t, repo, mail, nil)
	email := "http@example.com"
	if w := doJSON(t, svc.ServePolicy, http.MethodGet, RoutePolicy, ``); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"passwordLoginEnabled":true`) {
		t.Fatalf("policy: %d %s", w.Code, w.Body)
	}
	if w := doJSON(t, svc.ServeRegisterCode, http.MethodPost, RouteRegisterCode, `{"email":"`+email+`"}`); w.Code != http.StatusOK {
		t.Fatalf("register code: %d %s", w.Code, w.Body)
	}
	w := doJSON(t, svc.ServeRegister, http.MethodPost, RouteRegister, `{"email":"`+email+`","code":"`+mail.last().Code+`","password":"initial-password"}`)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 1 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	w = doJSON(t, svc.ServePassword, http.MethodPost, RoutePassword, `{"email":"`+email+`","password":"initial-password"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("password login: %d %s", w.Code, w.Body)
	}
	if w := doJSON(t, svc.ServeResetCode, http.MethodPost, RouteResetCode, `{"email":"`+email+`"}`); w.Code != http.StatusOK {
		t.Fatalf("reset code: %d %s", w.Code, w.Body)
	}
	w = doJSON(t, svc.ServeReset, http.MethodPost, RouteReset, `{"email":"`+email+`","code":"`+mail.last().Code+`","password":"replacement-password"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
}
