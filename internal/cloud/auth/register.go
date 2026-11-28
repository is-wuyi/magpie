package auth

import (
	"context"
	"strings"

	"github.com/yetone/magpie/internal/cloud/store"
)

// RegisterRequest proves control of a new address and chooses its password.
type RegisterRequest struct {
	Email     string
	Code      string
	Password  string
	IP        string
	UserAgent string
}

// RequestRegisterCode starts the explicit registration flow.
func (s *Service) RequestRegisterCode(ctx context.Context, req CodeRequest) (CodeSent, error) {
	return s.requestCode(ctx, req, PurposeRegister)
}

// Register consumes a registration code and creates the account and its first
// session. It never promotes an existing account or replaces its password.
func (s *Service) Register(ctx context.Context, req RegisterRequest) (LoginResult, error) {
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return LoginResult{}, err
	}
	policy, err := s.repo.AuthPolicy(ctx)
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	if !policy.RegistrationEnabled {
		return LoginResult{}, ErrRegistrationDisabled
	}
	if err := validatePassword(req.Password, policy.PasswordMinLength); err != nil {
		return LoginResult{}, err
	}
	rec, _, err := s.checkEmailCode(ctx, email, PurposeRegister, req.Code, req.IP)
	if err != nil {
		return LoginResult{}, err
	}
	hash, err := hashPassword(req.Password, policy.PasswordMinLength)
	if err != nil {
		return LoginResult{}, err
	}
	user, err := s.repo.CreateRegisteredUser(ctx, store.CreateRegisteredUserParams{
		Email: email, EmailNormalized: email, VerifiedAt: s.now(), PromoteAdmin: s.isAdmin(email),
		CodeID: rec.ID, CodeHash: s.hashLoginCode(PurposeRegister, email, strings.TrimSpace(req.Code)),
		PasswordHash: hash, PasswordHashVersion: passwordHashVersion,
	})
	if err != nil {
		return LoginResult{}, mapStoreError(err)
	}
	return s.completeLogin(ctx, user, LoginRequest{Email: email, IP: req.IP, UserAgent: req.UserAgent})
}
