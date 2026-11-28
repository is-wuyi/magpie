package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuthPolicy is the operator-controlled account policy. Deployment secrets
// stay outside this row: it contains only switches and the password length.
type AuthPolicy struct {
	RegistrationEnabled  bool      `json:"registrationEnabled"`
	PasswordLoginEnabled bool      `json:"passwordLoginEnabled"`
	CodeLoginEnabled     bool      `json:"codeLoginEnabled"`
	PasswordMinLength    int       `json:"passwordMinLength"`
	Version              int64     `json:"version"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

// UserCredentials adds a password verifier to a user's public identity. The
// hash never enters User or its HTTP representation.
type UserCredentials struct {
	User                User
	PasswordHash        string
	PasswordHashVersion int16
}

// CreateRegisteredUserParams is one verified registration. CodeID and CodeHash
// prove the register code; PasswordHash was computed by the auth package.
type CreateRegisteredUserParams struct {
	Email               string
	EmailNormalized     string
	VerifiedAt          time.Time
	PromoteAdmin        bool
	CodeID              uuid.UUID
	CodeHash            []byte
	PasswordHash        string
	PasswordHashVersion int16
}

// ResetPasswordParams proves a reset code and changes the password in one
// transaction. Advancing AuthVersion invalidates every older session.
type ResetPasswordParams struct {
	EmailNormalized     string
	CodeID              uuid.UUID
	CodeHash            []byte
	PasswordHash        string
	PasswordHashVersion int16
	Now                 time.Time
}

func validateAuthPolicy(p AuthPolicy) error {
	if !p.PasswordLoginEnabled && !p.CodeLoginEnabled {
		return fmt.Errorf("%w: at least one sign-in method must stay enabled", ErrInvalidArgument)
	}
	if p.PasswordMinLength < 8 || p.PasswordMinLength > 72 {
		return fmt.Errorf("%w: password minimum length must be between 8 and 72", ErrInvalidArgument)
	}
	if p.Version <= 0 {
		return fmt.Errorf("%w: policy version is required", ErrInvalidArgument)
	}
	return nil
}

// AuthPolicy returns the current policy. A missing or unreadable row is an
// error, never permission to assume registration or login is open.
func (s *Store) AuthPolicy(ctx context.Context) (AuthPolicy, error) {
	if s == nil || s.pool == nil {
		return AuthPolicy{}, ErrClosed
	}
	var p AuthPolicy
	err := s.pool.QueryRow(ctx, `SELECT registration_enabled, password_login_enabled, code_login_enabled,
		password_min_length, version, updated_at FROM auth_policy WHERE id = 1`).Scan(
		&p.RegistrationEnabled, &p.PasswordLoginEnabled, &p.CodeLoginEnabled, &p.PasswordMinLength, &p.Version, &p.UpdatedAt)
	return p, err
}

// SetAuthPolicy changes a policy only if Version is still the one the admin
// read, so two administrators cannot silently overwrite each other's work.
func (s *Store) SetAuthPolicy(ctx context.Context, p AuthPolicy, actorID uuid.UUID, now time.Time) (AuthPolicy, error) {
	if s == nil || s.pool == nil {
		return AuthPolicy{}, ErrClosed
	}
	if err := validateAuthPolicy(p); err != nil {
		return AuthPolicy{}, err
	}
	if now.IsZero() {
		return AuthPolicy{}, fmt.Errorf("%w: policy update time is required", ErrInvalidArgument)
	}
	if !p.CodeLoginEnabled {
		var admins int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND password_hash IS NULL`).Scan(&admins); err != nil {
			return AuthPolicy{}, err
		}
		if admins > 0 {
			return AuthPolicy{}, fmt.Errorf("%w: every administrator needs a password before code login is disabled", ErrInvalidArgument)
		}
	}
	var out AuthPolicy
	err := s.pool.QueryRow(ctx, `UPDATE auth_policy SET registration_enabled = $2,
		password_login_enabled = $3, code_login_enabled = $4, password_min_length = $5,
		version = version + 1, updated_at = $6, updated_by = $7
		WHERE id = 1 AND version = $1
		RETURNING registration_enabled, password_login_enabled, code_login_enabled, password_min_length, version, updated_at`,
		p.Version, p.RegistrationEnabled, p.PasswordLoginEnabled, p.CodeLoginEnabled, p.PasswordMinLength, now, actorID).Scan(
		&out.RegistrationEnabled, &out.PasswordLoginEnabled, &out.CodeLoginEnabled, &out.PasswordMinLength, &out.Version, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthPolicy{}, ErrPolicyConflict
	}
	return out, err
}

// UsersWithoutPassword reports accounts that still need email-code login or a
// reset before password-only authentication can be enforced.
func (s *Store) UsersWithoutPassword(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil {
		return 0, ErrClosed
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE password_hash IS NULL`).Scan(&n)
	return n, err
}

// CreateRegisteredUser consumes the registration code and creates the account
// atomically. An existing address is never promoted, overwritten or given a
// new password by a second registration.
func (s *Store) CreateRegisteredUser(ctx context.Context, p CreateRegisteredUserParams) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrClosed
	}
	if strings.TrimSpace(p.Email) == "" || strings.ToLower(strings.TrimSpace(p.Email)) != p.EmailNormalized || !validNormalizedEmail(p.EmailNormalized) {
		return User{}, fmt.Errorf("%w: registration email is not canonical", ErrInvalidArgument)
	}
	if p.CodeID == uuid.Nil || len(p.CodeHash) != hashSize || len(p.PasswordHash) < 20 || len(p.PasswordHash) > 512 || p.PasswordHashVersion <= 0 || p.VerifiedAt.IsZero() {
		return User{}, fmt.Errorf("%w: incomplete registration", ErrInvalidArgument)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE email_codes SET consumed_at = $2
		WHERE id = $1 AND code_hash = $3 AND consumed_at IS NULL AND attempts < max_attempts AND expires_at > $2`, p.CodeID, p.VerifiedAt, p.CodeHash)
	if err != nil {
		return User{}, err
	}
	if tag.RowsAffected() != 1 {
		return User{}, ErrCodeUnavailable
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return User{}, err
	}
	u, err := scanUser(tx.QueryRow(ctx, `INSERT INTO users
		(id, email, email_normalized, role, status, auth_version, email_verified_at, created_at, updated_at,
		 password_hash, password_hash_version, password_updated_at)
		VALUES ($1, $2, $3, CASE WHEN $5 THEN 'admin' ELSE 'user' END, 'active', 1, $4, $4, $4, $6, $7, $4)
		ON CONFLICT (email_normalized) DO NOTHING RETURNING `+userColumns,
		id, p.Email, p.EmailNormalized, p.VerifiedAt, p.PromoteAdmin, p.PasswordHash, p.PasswordHashVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

// CredentialsByEmail returns the password verifier for a sign-in or reset.
func (s *Store) CredentialsByEmail(ctx context.Context, emailNormalized string) (UserCredentials, error) {
	if s == nil || s.pool == nil {
		return UserCredentials{}, ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) {
		return UserCredentials{}, fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	var out UserCredentials
	var role, status string
	err := s.pool.QueryRow(ctx, `SELECT `+userColumns+`, COALESCE(password_hash, ''), COALESCE(password_hash_version, 0)
		FROM users WHERE email_normalized = $1`, emailNormalized).Scan(
		&out.User.ID, &out.User.Email, &out.User.EmailNormalized, &role, &status, &out.User.AuthVersion,
		&out.User.EmailVerifiedAt, &out.User.CreatedAt, &out.User.UpdatedAt, &out.PasswordHash, &out.PasswordHashVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserCredentials{}, ErrNotFound
	}
	out.User.Role, out.User.Status = Role(role), Status(status)
	return out, err
}

// ResetPassword consumes the reset code and changes the password atomically.
// AuthVersion invalidates all existing sessions, including the one used to ask
// for the reset; the caller issues a fresh session after this returns.
func (s *Store) ResetPassword(ctx context.Context, p ResetPasswordParams) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrClosed
	}
	if !validNormalizedEmail(p.EmailNormalized) || p.CodeID == uuid.Nil || len(p.CodeHash) != hashSize || len(p.PasswordHash) < 20 || len(p.PasswordHash) > 512 || p.PasswordHashVersion <= 0 || p.Now.IsZero() {
		return User{}, fmt.Errorf("%w: incomplete password reset", ErrInvalidArgument)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, `UPDATE email_codes SET consumed_at = $2
		WHERE id = $1 AND code_hash = $3 AND consumed_at IS NULL AND attempts < max_attempts AND expires_at > $2`, p.CodeID, p.Now, p.CodeHash)
	if err != nil {
		return User{}, err
	}
	if tag.RowsAffected() != 1 {
		return User{}, ErrCodeUnavailable
	}
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET password_hash = $2, password_hash_version = $3,
		password_updated_at = $4, auth_version = auth_version + 1, updated_at = $4
		WHERE email_normalized = $1 AND status = 'active' RETURNING `+userColumns,
		p.EmailNormalized, p.PasswordHash, p.PasswordHashVersion, p.Now))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, u.ID, p.Now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

// RecordAuthAttempt stores one failed password login for sliding-window
// throttling. It is deliberately separate from email-code failures.
func (s *Store) RecordAuthAttempt(ctx context.Context, emailNormalized, requestIP string, now time.Time) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) || now.IsZero() {
		return fmt.Errorf("%w: invalid password attempt", ErrInvalidArgument)
	}
	ip, err := parseIP(requestIP)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO auth_attempts (id, kind, email_normalized, request_ip, created_at)
		VALUES ($1, 'password', $2, $3, $4)`, uuid.New(), emailNormalized, ip, now)
	return err
}

// AuthFailureCounts counts password failures since a timestamp.
func (s *Store) AuthFailureCounts(ctx context.Context, emailNormalized, requestIP string, since time.Time) (FailureCounts, error) {
	if s == nil || s.pool == nil {
		return FailureCounts{}, ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) || since.IsZero() {
		return FailureCounts{}, fmt.Errorf("%w: invalid failure window", ErrInvalidArgument)
	}
	ip, err := parseIP(requestIP)
	if err != nil {
		return FailureCounts{}, err
	}
	var out FailureCounts
	if err := s.pool.QueryRow(ctx, `SELECT
		COALESCE(sum(CASE WHEN email_normalized = $1 THEN 1 ELSE 0 END), 0),
		COALESCE(sum(CASE WHEN request_ip = $2::inet THEN 1 ELSE 0 END), 0)
		FROM auth_attempts WHERE kind = 'password' AND created_at > $3`, emailNormalized, ip, since).Scan(&out.ByEmail, &out.ByIP); err != nil {
		return FailureCounts{}, err
	}
	return out, nil
}

// ClearAuthAttempts forgets password failures after a successful login.
func (s *Store) ClearAuthAttempts(ctx context.Context, emailNormalized string) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) {
		return fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_attempts WHERE kind = 'password' AND email_normalized = $1`, emailNormalized)
	return err
}

// CleanupAuthAttempts removes rows that can no longer affect a sliding window.
func (s *Store) CleanupAuthAttempts(ctx context.Context, before time.Time) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if before.IsZero() {
		return fmt.Errorf("%w: cleanup cutoff is required", ErrInvalidArgument)
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM auth_attempts WHERE created_at < $1`, before)
	return err
}
