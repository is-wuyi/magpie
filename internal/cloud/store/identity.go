package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Role is a user's authority. Only an operator's explicit admin list may
// promote a user to RoleAdmin, and only after the address was verified.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// Status is whether a user may sign in at all.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// User is a cloud account. Email is the address as the user last typed it;
// EmailNormalized is the unique key every lookup uses.
type User struct {
	ID              uuid.UUID
	Email           string
	EmailNormalized string
	Role            Role
	Status          Status
	AuthVersion     int
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EmailCode is one one-time sign-in code. CodeHash is an HMAC digest; the
// six digits themselves are never stored. Attempts counts failed
// verifications, and ConsumedAt makes the row unusable in one atomic step.
type EmailCode struct {
	ID              uuid.UUID
	EmailNormalized string
	Purpose         string
	CodeHash        []byte
	HashVersion     int16
	Attempts        int16
	MaxAttempts     int16
	CreatedAt       time.Time
	ExpiresAt       time.Time
	ConsumedAt      *time.Time
	RequestIP       string
}

// EmailCodeAttempt is the result of recording a wrong code.
type EmailCodeAttempt struct {
	Attempts    int16
	MaxAttempts int16
	Exhausted   bool
}

// FailureCounts is the failed-attempt total inside a throttle window.
type FailureCounts struct {
	ByEmail int
	ByIP    int
}

// Session is a signed-in browser. TokenHash is an HMAC digest of the random
// token held in the cookie; the token itself is not stored.
type Session struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	TokenHash   []byte
	AuthVersion int
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	UserAgent   string
	RequestIP   string
}

// EnsureVerifiedUserParams creates a user, or updates the existing one, for
// an address whose ownership was just proven. PromoteAdmin must be true only
// when the normalized address is on the operator's explicit admin list.
type EnsureVerifiedUserParams struct {
	Email           string
	EmailNormalized string
	VerifiedAt      time.Time
	PromoteAdmin    bool
}

// IssueEmailCodeParams is one send request. The throttle fields are
// evaluated inside the same transaction and under a per-email advisory
// lock, so two concurrent sends cannot both pass.
type IssueEmailCodeParams struct {
	EmailNormalized string
	Purpose         string
	CodeHash        []byte
	HashVersion     int16
	MaxAttempts     int16
	Now             time.Time
	ExpiresAt       time.Time
	RequestIP       string

	Window time.Duration

	SendInterval     time.Duration
	MaxSendsPerEmail int
	MaxSendsPerIP    int
}

// CreateSessionParams stores a new session. AuthVersion is the user's
// version at sign-in; a session whose version no longer matches the user's
// is invalid.
type CreateSessionParams struct {
	UserID      uuid.UUID
	TokenHash   []byte
	AuthVersion int
	Now         time.Time
	ExpiresAt   time.Time
	UserAgent   string
	RequestIP   string
}

const (
	maxEmailLength   = 254
	maxPurposeLength = 32
	maxUserAgentLen  = 512
	hashSize         = 32
)

const userColumns = `id, email, email_normalized, role, status, auth_version, email_verified_at, created_at, updated_at`

const emailCodeColumns = `id, email_normalized, purpose, code_hash, hash_version, attempts, max_attempts, created_at, expires_at, consumed_at, COALESCE(host(request_ip), '')`

const sessionColumns = `id, user_id, token_hash, auth_version, created_at, last_seen_at, expires_at, revoked_at, COALESCE(user_agent, ''), COALESCE(host(request_ip), '')`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var u User
	var role, status string
	if err := row.Scan(&u.ID, &u.Email, &u.EmailNormalized, &role, &status, &u.AuthVersion,
		&u.EmailVerifiedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return User{}, err
	}
	u.Role = Role(role)
	u.Status = Status(status)
	return u, nil
}

func scanEmailCode(row rowScanner) (EmailCode, error) {
	var c EmailCode
	if err := row.Scan(&c.ID, &c.EmailNormalized, &c.Purpose, &c.CodeHash, &c.HashVersion,
		&c.Attempts, &c.MaxAttempts, &c.CreatedAt, &c.ExpiresAt, &c.ConsumedAt, &c.RequestIP); err != nil {
		return EmailCode{}, err
	}
	return c, nil
}

func scanSession(row rowScanner) (Session, error) {
	var s Session
	if err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.AuthVersion, &s.CreatedAt,
		&s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt, &s.UserAgent, &s.RequestIP); err != nil {
		return Session{}, err
	}
	return s, nil
}

// EnsureVerifiedUser creates an ordinary user or returns the existing one,
// ending with an admin role only when PromoteAdmin is set. A disabled user
// is refused; nothing about their row is changed.
func (s *Store) EnsureVerifiedUser(ctx context.Context, p EnsureVerifiedUserParams) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrClosed
	}
	if !validNormalizedEmail(p.EmailNormalized) {
		return User{}, fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	if strings.TrimSpace(p.Email) == "" || len(p.Email) > maxEmailLength || hasControl(p.Email) ||
		strings.ToLower(strings.TrimSpace(p.Email)) != p.EmailNormalized {
		return User{}, fmt.Errorf("%w: email does not match email_normalized", ErrInvalidArgument)
	}
	if p.VerifiedAt.IsZero() {
		return User{}, fmt.Errorf("%w: verified time is required", ErrInvalidArgument)
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return User{}, fmt.Errorf("store: generate user id: %w", err)
	}
	const q = `
INSERT INTO users (id, email, email_normalized, role, status, auth_version, email_verified_at, created_at, updated_at)
VALUES ($1, $2, $3, CASE WHEN $5 THEN 'admin' ELSE 'user' END, 'active', 1, $4, $6, $7)
ON CONFLICT (email_normalized) DO UPDATE SET
    email = EXCLUDED.email,
    email_verified_at = COALESCE(users.email_verified_at, EXCLUDED.email_verified_at),
    role = CASE WHEN $5 THEN 'admin' ELSE users.role END,
    updated_at = EXCLUDED.updated_at
WHERE users.status = 'active'
RETURNING ` + userColumns
	u, err := scanUser(s.pool.QueryRow(ctx, q, id, p.Email, p.EmailNormalized, p.VerifiedAt,
		p.PromoteAdmin, p.VerifiedAt, p.VerifiedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserDisabled
	}
	if err != nil {
		return User{}, fmt.Errorf("store: ensure user: %w", err)
	}
	return u, nil
}

// UserByID returns a user, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrClosed
	}
	if id == uuid.Nil {
		return User{}, fmt.Errorf("%w: user id is empty", ErrInvalidArgument)
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: read user: %w", err)
	}
	return u, nil
}

// UserByEmail returns a user by normalized address, or ErrNotFound.
func (s *Store) UserByEmail(ctx context.Context, emailNormalized string) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) {
		return User{}, fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email_normalized = $1`, emailNormalized))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: read user: %w", err)
	}
	return u, nil
}

// IssueEmailCode stores a new code after checking the send throttle and
// invalidating the previous live code for the same address and purpose. All
// three steps run in one transaction under a per-email advisory lock.
func (s *Store) IssueEmailCode(ctx context.Context, p IssueEmailCodeParams) (EmailCode, error) {
	if s == nil || s.pool == nil {
		return EmailCode{}, ErrClosed
	}
	if err := validateIssueEmailCode(p); err != nil {
		return EmailCode{}, err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return EmailCode{}, fmt.Errorf("store: generate code id: %w", err)
	}
	ip, err := parseIP(p.RequestIP)
	if err != nil {
		return EmailCode{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EmailCode{}, fmt.Errorf("store: begin issue code: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// One send at a time per address, across every process.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, emailLockKey(p.EmailNormalized)); err != nil {
		return EmailCode{}, fmt.Errorf("store: lock email for code issue: %w", err)
	}

	if err := checkSendThrottle(ctx, tx, p, ip); err != nil {
		return EmailCode{}, err
	}

	if _, err := tx.Exec(ctx, `
UPDATE email_codes SET consumed_at = $3
WHERE email_normalized = $1 AND purpose = $2
  AND consumed_at IS NULL AND attempts < max_attempts`, p.EmailNormalized, p.Purpose, p.Now); err != nil {
		return EmailCode{}, fmt.Errorf("store: invalidate previous code: %w", err)
	}

	c, err := scanEmailCode(tx.QueryRow(ctx, `
INSERT INTO email_codes
    (id, email_normalized, purpose, code_hash, hash_version, attempts, max_attempts, created_at, expires_at, request_ip)
VALUES ($1, $2, $3, $4, $5, 0, $6, $7, $8, $9)
RETURNING `+emailCodeColumns,
		id, p.EmailNormalized, p.Purpose, p.CodeHash, p.HashVersion, p.MaxAttempts, p.Now, p.ExpiresAt, ip))
	if err != nil {
		return EmailCode{}, fmt.Errorf("store: insert email code: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return EmailCode{}, fmt.Errorf("store: commit email code: %w", err)
	}
	return c, nil
}

func checkSendThrottle(ctx context.Context, tx pgx.Tx, p IssueEmailCodeParams, ip netip.Addr) error {
	var retry time.Duration
	consider := func(d time.Duration) {
		if d < time.Second {
			d = time.Second
		}
		if d > retry {
			retry = d
		}
	}

	if p.Window > 0 && (p.SendInterval > 0 || p.MaxSendsPerEmail > 0) {
		var count int64
		var first, last time.Time
		if err := tx.QueryRow(ctx, `
SELECT count(*), COALESCE(min(created_at), $2::timestamptz), COALESCE(max(created_at), $2::timestamptz)
FROM email_codes
WHERE email_normalized = $1 AND created_at > $2::timestamptz - $3::interval`,
			p.EmailNormalized, p.Now, p.Window).Scan(&count, &first, &last); err != nil {
			return fmt.Errorf("store: read send throttle: %w", err)
		}
		if count > 0 && p.SendInterval > 0 {
			consider(p.SendInterval - p.Now.Sub(last))
		}
		if p.MaxSendsPerEmail > 0 && count >= int64(p.MaxSendsPerEmail) {
			consider(first.Add(p.Window).Sub(p.Now))
		}
	}

	if p.Window > 0 && p.MaxSendsPerIP > 0 && ip.IsValid() {
		var count int64
		var first time.Time
		if err := tx.QueryRow(ctx, `
SELECT count(*), COALESCE(min(created_at), $2::timestamptz)
FROM email_codes
WHERE request_ip = $1::inet AND created_at > $2::timestamptz - $3::interval`,
			ip, p.Now, p.Window).Scan(&count, &first); err != nil {
			return fmt.Errorf("store: read send throttle: %w", err)
		}
		if count >= int64(p.MaxSendsPerIP) {
			consider(first.Add(p.Window).Sub(p.Now))
		}
	}

	if retry > 0 {
		return &RateLimitError{RetryAfter: retry}
	}
	return nil
}

// LatestActiveEmailCode returns the one usable code for an address and
// purpose, or ErrNotFound. It is the latest send; issuing a new code makes
// every older one unusable.
func (s *Store) LatestActiveEmailCode(ctx context.Context, emailNormalized, purpose string, now time.Time) (EmailCode, error) {
	if s == nil || s.pool == nil {
		return EmailCode{}, ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) {
		return EmailCode{}, fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	if !validPurpose(purpose) {
		return EmailCode{}, fmt.Errorf("%w: purpose is not usable", ErrInvalidArgument)
	}
	if now.IsZero() {
		return EmailCode{}, fmt.Errorf("%w: now is required", ErrInvalidArgument)
	}
	c, err := scanEmailCode(s.pool.QueryRow(ctx, `
SELECT `+emailCodeColumns+`
FROM email_codes
WHERE email_normalized = $1 AND purpose = $2
  AND consumed_at IS NULL AND attempts < max_attempts AND expires_at > $3
ORDER BY created_at DESC
LIMIT 1`, emailNormalized, purpose, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailCode{}, ErrNotFound
	}
	if err != nil {
		return EmailCode{}, fmt.Errorf("store: read email code: %w", err)
	}
	return c, nil
}

// RecordEmailCodeFailure counts a wrong code. Reaching max_attempts consumes
// the code in the same statement, so a brute-force run cannot continue past
// the limit even under concurrency.
func (s *Store) RecordEmailCodeFailure(ctx context.Context, id uuid.UUID, now time.Time) (EmailCodeAttempt, error) {
	if s == nil || s.pool == nil {
		return EmailCodeAttempt{}, ErrClosed
	}
	if id == uuid.Nil || now.IsZero() {
		return EmailCodeAttempt{}, fmt.Errorf("%w: code id and time are required", ErrInvalidArgument)
	}
	var a EmailCodeAttempt
	err := s.pool.QueryRow(ctx, `
UPDATE email_codes
SET attempts = attempts + 1,
    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN $2 ELSE consumed_at END
WHERE id = $1 AND consumed_at IS NULL AND attempts < max_attempts
RETURNING attempts, max_attempts`, id, now).Scan(&a.Attempts, &a.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailCodeAttempt{}, ErrCodeUnavailable
	}
	if err != nil {
		return EmailCodeAttempt{}, fmt.Errorf("store: record code failure: %w", err)
	}
	a.Exhausted = a.Attempts >= a.MaxAttempts
	return a, nil
}

// ConsumeEmailCode marks a verified code used in one atomic update. It
// returns false with ErrCodeUnavailable when the code is already used,
// expired, exhausted, or no longer matches codeHash.
func (s *Store) ConsumeEmailCode(ctx context.Context, id uuid.UUID, codeHash []byte, now time.Time) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if id == uuid.Nil || len(codeHash) != hashSize || now.IsZero() {
		return fmt.Errorf("%w: code id, hash and time are required", ErrInvalidArgument)
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE email_codes SET consumed_at = $3
WHERE id = $1 AND code_hash = $2
  AND consumed_at IS NULL AND attempts < max_attempts AND expires_at > $3`, id, codeHash, now)
	if err != nil {
		return fmt.Errorf("store: consume email code: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCodeUnavailable
	}
	return nil
}

// DeleteEmailCode removes a code that could not be delivered. It is
// idempotent and never reports a missing row.
func (s *Store) DeleteEmailCode(ctx context.Context, id uuid.UUID) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: code id is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM email_codes WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete email code: %w", err)
	}
	return nil
}

// EmailCodeFailureCounts sums the wrong attempts for an address and an
// address+IP since the given time. A store error is returned; it is never
// read as "no failures".
func (s *Store) EmailCodeFailureCounts(ctx context.Context, emailNormalized, requestIP string, since time.Time) (FailureCounts, error) {
	if s == nil || s.pool == nil {
		return FailureCounts{}, ErrClosed
	}
	if !validNormalizedEmail(emailNormalized) {
		return FailureCounts{}, fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	if since.IsZero() {
		return FailureCounts{}, fmt.Errorf("%w: since is required", ErrInvalidArgument)
	}
	ip, err := parseIP(requestIP)
	if err != nil {
		return FailureCounts{}, err
	}
	var byEmail, byIP int64
	if err := s.pool.QueryRow(ctx, `
SELECT
    COALESCE(sum(attempts) FILTER (WHERE email_normalized = $1), 0),
    COALESCE(sum(attempts) FILTER (WHERE request_ip = $2::inet), 0)
FROM email_codes
WHERE created_at > $3
  AND (email_normalized = $1 OR request_ip = $2::inet)`, emailNormalized, ip, since).Scan(&byEmail, &byIP); err != nil {
		return FailureCounts{}, fmt.Errorf("store: read verify throttle: %w", err)
	}
	return FailureCounts{ByEmail: int(byEmail), ByIP: int(byIP)}, nil
}

// CreateSession stores a session for an active, verified user whose
// auth_version still matches. A stale or disabled user yields
// ErrSessionStale.
func (s *Store) CreateSession(ctx context.Context, p CreateSessionParams) (Session, error) {
	if s == nil || s.pool == nil {
		return Session{}, ErrClosed
	}
	if p.UserID == uuid.Nil || len(p.TokenHash) != hashSize || p.AuthVersion <= 0 {
		return Session{}, fmt.Errorf("%w: user id, token hash and auth version are required", ErrInvalidArgument)
	}
	if p.Now.IsZero() || !p.ExpiresAt.After(p.Now) {
		return Session{}, fmt.Errorf("%w: session expiry must be after its start", ErrInvalidArgument)
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return Session{}, fmt.Errorf("store: generate session id: %w", err)
	}
	ip, err := parseIP(p.RequestIP)
	if err != nil {
		return Session{}, err
	}
	ua := truncateText(strings.ToValidUTF8(p.UserAgent, ""), maxUserAgentLen)
	sess, err := scanSession(s.pool.QueryRow(ctx, `
INSERT INTO user_sessions
    (id, user_id, token_hash, auth_version, created_at, last_seen_at, expires_at, user_agent, request_ip)
SELECT $1, u.id, $2, u.auth_version, $4, $4, $5, $6, $7
FROM users u
WHERE u.id = $3 AND u.status = 'active'
  AND u.email_verified_at IS NOT NULL AND u.auth_version = $8
RETURNING `+sessionColumns,
		id, p.TokenHash, p.UserID, p.Now, p.ExpiresAt, ua, ip, p.AuthVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSessionStale
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
	}
	return sess, nil
}

// SessionByTokenHash returns a live session and its user, or ErrNotFound.
// A revoked session, an expired one, a disabled user, an unverified address
// or an outdated auth version all read as not found, so a caller has one
// fail-closed answer.
func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) (Session, User, error) {
	if s == nil || s.pool == nil {
		return Session{}, User{}, ErrClosed
	}
	if len(tokenHash) != hashSize || now.IsZero() {
		return Session{}, User{}, fmt.Errorf("%w: token hash and time are required", ErrInvalidArgument)
	}
	row := s.pool.QueryRow(ctx, `
SELECT s.id, s.user_id, s.token_hash, s.auth_version, s.created_at, s.last_seen_at, s.expires_at,
       s.revoked_at, COALESCE(s.user_agent, ''), COALESCE(host(s.request_ip), ''),
       u.id, u.email, u.email_normalized, u.role, u.status, u.auth_version,
       u.email_verified_at, u.created_at, u.updated_at
FROM user_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.revoked_at IS NULL AND s.expires_at > $2
  AND s.auth_version = u.auth_version
  AND u.status = 'active' AND u.email_verified_at IS NOT NULL
LIMIT 1`, tokenHash, now)
	var sess Session
	var u User
	var role, status string
	err := row.Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.AuthVersion, &sess.CreatedAt,
		&sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt, &sess.UserAgent, &sess.RequestIP,
		&u.ID, &u.Email, &u.EmailNormalized, &role, &status, &u.AuthVersion,
		&u.EmailVerifiedAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, User{}, ErrNotFound
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("store: read session: %w", err)
	}
	u.Role = Role(role)
	u.Status = Status(status)
	return sess, u, nil
}

// TouchSession updates last_seen_at, at most once per minInterval. A failed
// touch is returned to the caller, which decides whether it matters; it is
// never read as a revocation.
func (s *Store) TouchSession(ctx context.Context, id uuid.UUID, now time.Time, minInterval time.Duration) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if id == uuid.Nil || now.IsZero() {
		return fmt.Errorf("%w: session id and time are required", ErrInvalidArgument)
	}
	if minInterval < 0 {
		minInterval = 0
	}
	_, err := s.pool.Exec(ctx, `
UPDATE user_sessions SET last_seen_at = $2
WHERE id = $1 AND revoked_at IS NULL AND expires_at > $2
  AND last_seen_at <= $2::timestamptz - $3::interval`, id, now, minInterval)
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// RevokeSession revokes one session by id, or returns ErrNotFound.
func (s *Store) RevokeSession(ctx context.Context, id uuid.UUID, now time.Time) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if id == uuid.Nil || now.IsZero() {
		return fmt.Errorf("%w: session id and time are required", ErrInvalidArgument)
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE user_sessions SET revoked_at = $2
WHERE id = $1 AND revoked_at IS NULL`, id, now)
	if err != nil {
		return fmt.Errorf("store: revoke session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// RevokeSessionByTokenHash revokes the session a cookie token names, or
// returns ErrNotFound. Logout treats that as already done.
func (s *Store) RevokeSessionByTokenHash(ctx context.Context, tokenHash []byte, now time.Time) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	if len(tokenHash) != hashSize || now.IsZero() {
		return fmt.Errorf("%w: token hash and time are required", ErrInvalidArgument)
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE user_sessions SET revoked_at = $2
WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash, now)
	if err != nil {
		return fmt.Errorf("store: revoke session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserSessions revokes every live session of a user and raises the
// user's auth_version, then returns the new version. Raising the version is
// what makes an older session unusable even if a row was missed.
func (s *Store) RevokeUserSessions(ctx context.Context, userID uuid.UUID, now time.Time) (int, error) {
	if s == nil || s.pool == nil {
		return 0, ErrClosed
	}
	if userID == uuid.Nil || now.IsZero() {
		return 0, fmt.Errorf("%w: user id and time are required", ErrInvalidArgument)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin revoke all: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var version int
	err = tx.QueryRow(ctx, `
UPDATE users SET auth_version = auth_version + 1, updated_at = $2
WHERE id = $1
RETURNING auth_version`, userID, now).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: raise auth version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE user_sessions SET revoked_at = $2
WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
		return 0, fmt.Errorf("store: revoke sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit revoke all: %w", err)
	}
	return version, nil
}

func validateIssueEmailCode(p IssueEmailCodeParams) error {
	if !validNormalizedEmail(p.EmailNormalized) {
		return fmt.Errorf("%w: email_normalized is not canonical", ErrInvalidArgument)
	}
	if !validPurpose(p.Purpose) {
		return fmt.Errorf("%w: purpose is not usable", ErrInvalidArgument)
	}
	if len(p.CodeHash) != hashSize {
		return fmt.Errorf("%w: code hash must be %d bytes", ErrInvalidArgument, hashSize)
	}
	if p.HashVersion <= 0 || p.MaxAttempts <= 0 {
		return fmt.Errorf("%w: hash version and max attempts must be positive", ErrInvalidArgument)
	}
	if p.Now.IsZero() || !p.ExpiresAt.After(p.Now) {
		return fmt.Errorf("%w: code expiry must be after its start", ErrInvalidArgument)
	}
	if p.Window < 0 || p.SendInterval < 0 || p.MaxSendsPerEmail < 0 || p.MaxSendsPerIP < 0 {
		return fmt.Errorf("%w: throttle values must not be negative", ErrInvalidArgument)
	}
	return nil
}

// validNormalizedEmail checks the canonical form the users and email_codes
// constraints also require. The auth package produces it; this keeps store
// from writing a non-canonical address that would make the unique key
// misleading.
func validNormalizedEmail(s string) bool {
	if len(s) < 3 || len(s) > maxEmailLength || s != strings.ToLower(s) || strings.TrimSpace(s) != s {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.IndexByte(s[at+1:], '@') >= 0 {
		return false
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validPurpose(s string) bool {
	if len(s) < 3 || len(s) > maxPurposeLength {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '_' && i > 0:
		default:
			return false
		}
	}
	return true
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// parseIP turns a request address into the value pgx sends for an inet
// column. An empty address is NULL; a malformed one is refused rather than
// dropped, since silently losing it would disable the per-IP throttle.
func parseIP(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%w: request IP is not an address", ErrInvalidArgument)
	}
	return addr, nil
}

// emailLockKey is the per-email advisory lock. A hash collision only makes
// two sends wait for each other, which is safe.
func emailLockKey(email string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(email))
	return int64(h.Sum64())
}
