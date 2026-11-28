package auth

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/yetone/magpie/internal/cloud/store"
)

const passwordHashVersion int16 = 1
const bcryptCost = 12

var dummyPasswordHash = func() string {
	hash, _ := bcrypt.GenerateFromPassword([]byte("not-the-account-password"), bcryptCost)
	return string(hash)
}()

// PasswordLoginRequest authenticates an existing account by password.
type PasswordLoginRequest struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// ResetPasswordRequest proves control of the email and chooses a new
// password. It never returns a generated password to the caller.
type ResetPasswordRequest struct {
	Email     string
	Code      string
	Password  string
	IP        string
	UserAgent string
}

func validatePassword(password string, minLength int) error {
	if minLength < 8 || minLength > 72 || len(password) < minLength {
		return ErrInvalidPassword
	}
	// bcrypt silently truncates after 72 bytes. Reject rather than giving the
	// user a credential that differs from the one they think they chose.
	if len(password) > 72 {
		return ErrInvalidPassword
	}
	return nil
}

func hashPassword(password string, minLength int) (string, error) {
	if err := validatePassword(password, minLength); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// PasswordLogin verifies a password and issues a fresh session. Unknown,
// disabled and wrongly entered accounts share one public failure.
func (s *Service) PasswordLogin(ctx context.Context, req PasswordLoginRequest) (LoginResult, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return LoginResult{}, err
	}
	policy, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if !policy.PasswordLoginEnabled {
		return LoginResult{}, ErrPasswordLoginDisabled
	}
	now := s.now()
	ip := canonicalIP(req.IP)
	counts, err := s.repo.AuthFailureCounts(ctx, email, ip, now.Add(-throttleWindow))
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if counts.ByEmail >= s.cfg.VerifyFailuresPerHour || ip != "" && counts.ByIP >= s.cfg.VerifyFailuresPerIPPerHour {
		return LoginResult{}, &RateLimitError{RetryAfter: throttleWindow}
	}
	credentials, err := s.repo.CredentialsByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(req.Password))
		return s.recordPasswordFailure(ctx, email, ip)
	}
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if credentials.User.Status != store.StatusActive || credentials.PasswordHashVersion != passwordHashVersion || credentials.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(credentials.PasswordHash), []byte(req.Password)) != nil {
		return s.recordPasswordFailure(ctx, email, ip)
	}
	if err := s.repo.ClearAuthAttempts(ctx, email); err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	return s.completeLogin(ctx, credentials.User, LoginRequest{Email: email, IP: req.IP, UserAgent: req.UserAgent})
}

func (s *Service) recordPasswordFailure(ctx context.Context, email, ip string) (LoginResult, error) {
	if err := s.repo.RecordAuthAttempt(ctx, email, ip, s.now()); err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	return LoginResult{}, ErrInvalidCredentials
}

// RequestResetCode starts a password reset without revealing whether the
// address has an account.
func (s *Service) RequestResetCode(ctx context.Context, req CodeRequest) (CodeSent, error) {
	return s.requestCode(ctx, req, PurposeReset)
}

// ResetPassword consumes the reset code, replaces the password and revokes all
// older sessions before issuing a fresh one.
func (s *Service) ResetPassword(ctx context.Context, req ResetPasswordRequest) (LoginResult, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return LoginResult{}, err
	}
	policy, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if !policy.PasswordLoginEnabled {
		return LoginResult{}, ErrPasswordLoginDisabled
	}
	if err := validatePassword(req.Password, policy.PasswordMinLength); err != nil {
		return LoginResult{}, err
	}
	rec, _, err := s.checkEmailCode(ctx, email, PurposeReset, req.Code, req.IP)
	if err != nil {
		return LoginResult{}, err
	}
	hash, err := hashPassword(req.Password, policy.PasswordMinLength)
	if err != nil {
		return LoginResult{}, err
	}
	user, err := s.repo.ResetPassword(ctx, store.ResetPasswordParams{
		EmailNormalized: email, CodeID: rec.ID, CodeHash: s.hashLoginCode(PurposeReset, email, strings.TrimSpace(req.Code)),
		PasswordHash: hash, PasswordHashVersion: passwordHashVersion, Now: s.now(),
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCodeUnavailable) {
		return LoginResult{}, ErrInvalidCode
	}
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	return s.completeLogin(ctx, user, LoginRequest{Email: email, IP: req.IP, UserAgent: req.UserAgent})
}
