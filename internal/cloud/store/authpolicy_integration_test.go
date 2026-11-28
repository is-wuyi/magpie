package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrationAuthRegistrationPolicyAndReset(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	policy, err := s.AuthPolicy(ctx)
	if err != nil || !policy.RegistrationEnabled || !policy.PasswordLoginEnabled || !policy.CodeLoginEnabled || policy.PasswordMinLength != 12 {
		t.Fatalf("default policy: %+v, %v", policy, err)
	}
	email := "register@example.com"
	hash := make([]byte, 32)
	hash[0] = 9
	code, err := s.IssueEmailCode(ctx, IssueEmailCodeParams{EmailNormalized: email, Purpose: "register", CodeHash: hash,
		HashVersion: 1, MaxAttempts: 5, Now: now, ExpiresAt: now.Add(10 * time.Minute), RequestIP: "203.0.113.9", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.CreateRegisteredUser(ctx, CreateRegisteredUserParams{Email: email, EmailNormalized: email, VerifiedAt: now,
		CodeID: code.ID, CodeHash: hash, PasswordHash: "a bcrypt-shaped-password-hash", PasswordHashVersion: 1})
	if err != nil || user.EmailVerifiedAt == nil {
		t.Fatalf("registered: %+v, %v", user, err)
	}
	credentials, err := s.CredentialsByEmail(ctx, email)
	if err != nil || credentials.PasswordHash != "a bcrypt-shaped-password-hash" {
		t.Fatalf("credentials: %+v, %v", credentials, err)
	}

	// A second registration cannot consume its code before the unique email
	// conflict is known: the whole transaction rolls back.
	secondHash := make([]byte, 32)
	secondHash[0] = 10
	second, err := s.IssueEmailCode(ctx, IssueEmailCodeParams{EmailNormalized: email, Purpose: "register", CodeHash: secondHash,
		HashVersion: 1, MaxAttempts: 5, Now: now.Add(time.Second), ExpiresAt: now.Add(10 * time.Minute), RequestIP: "203.0.113.9", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRegisteredUser(ctx, CreateRegisteredUserParams{Email: email, EmailNormalized: email, VerifiedAt: now.Add(time.Second),
		CodeID: second.ID, CodeHash: secondHash, PasswordHash: "another-password-hash", PasswordHashVersion: 1}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate registration: %v", err)
	}
	if _, err := s.LatestActiveEmailCode(ctx, email, "register", now.Add(time.Second)); err != nil {
		t.Fatalf("duplicate consumed its code before failing: %v", err)
	}

	session, err := s.CreateSession(ctx, CreateSessionParams{UserID: user.ID, TokenHash: make([]byte, 32), AuthVersion: user.AuthVersion,
		Now: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	resetHash := make([]byte, 32)
	resetHash[0] = 11
	reset, err := s.IssueEmailCode(ctx, IssueEmailCodeParams{EmailNormalized: email, Purpose: "reset", CodeHash: resetHash,
		HashVersion: 1, MaxAttempts: 5, Now: now, ExpiresAt: now.Add(10 * time.Minute), RequestIP: "203.0.113.9", Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.ResetPassword(ctx, ResetPasswordParams{EmailNormalized: email, CodeID: reset.ID, CodeHash: resetHash,
		PasswordHash: "a-new-password-hash-value", PasswordHashVersion: 1, Now: now.Add(time.Minute)})
	if err != nil || updated.AuthVersion != user.AuthVersion+1 {
		t.Fatalf("reset: %+v, %v", updated, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, session.TokenHash, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old session survived reset: %v", err)
	}

	policy.CodeLoginEnabled = false
	changed, err := s.SetAuthPolicy(ctx, policy, user.ID, now)
	if err != nil || changed.Version != policy.Version+1 {
		t.Fatalf("policy update: %+v, %v", changed, err)
	}
	if _, err := s.SetAuthPolicy(ctx, policy, user.ID, now); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("stale policy update: %v", err)
	}
	changed.PasswordLoginEnabled = false
	if _, err := s.SetAuthPolicy(ctx, changed, user.ID, now); err == nil {
		t.Fatal("both login methods were disabled")
	}
}

func TestIntegrationAuthAttemptCounts(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	email := "attempt@example.com"
	if err := s.RecordAuthAttempt(ctx, email, "203.0.113.9", now); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AuthFailureCounts(ctx, email, "203.0.113.9", now.Add(-time.Hour))
	if err != nil || counts.ByEmail != 1 || counts.ByIP != 1 {
		t.Fatalf("counts: %+v, %v", counts, err)
	}
	if err := s.ClearAuthAttempts(ctx, email); err != nil {
		t.Fatal(err)
	}
	counts, err = s.AuthFailureCounts(ctx, email, "203.0.113.9", now.Add(-time.Hour))
	if err != nil || counts.ByEmail != 0 {
		t.Fatalf("cleared counts: %+v, %v", counts, err)
	}
	if err := s.RecordAuthAttempt(ctx, email, "203.0.113.9", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupAuthAttempts(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	counts, err = s.AuthFailureCounts(ctx, email, "203.0.113.9", now.Add(-3*time.Hour))
	if err != nil || counts.ByEmail != 0 {
		t.Fatalf("cleanup counts: %+v, %v", counts, err)
	}
}

func TestIntegrationCodeLoginCannotDisablePasswordlessAdmin(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	admin, err := s.EnsureVerifiedUser(ctx, EnsureVerifiedUserParams{Email: "admin@example.com", EmailNormalized: "admin@example.com", VerifiedAt: now, PromoteAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.AuthPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy.CodeLoginEnabled = false
	if _, err := s.SetAuthPolicy(ctx, policy, admin.ID, now); err == nil {
		t.Fatal("the only passwordless administrator was locked out")
	}
	if n, err := s.UsersWithoutPassword(ctx); err != nil || n != 1 {
		t.Fatalf("passwordless users %d, %v", n, err)
	}
}
