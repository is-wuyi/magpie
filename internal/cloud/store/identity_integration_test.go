package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// integrationStore gives a test its own schema in MAGPIE_TEST_DATABASE_URL's
// database. Without that variable every test here skips, so the package's
// default run needs no PostgreSQL and no network.
func integrationStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MAGPIE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set MAGPIE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect MAGPIE_TEST_DATABASE_URL: %v", err)
	}
	schema := "magpie_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop test schema %s: %v", schema, err)
		}
		_ = admin.Close(cleanupCtx)
	})

	s, err := Open(ctx, Config{URL: dsn, SearchPath: schema, MaxConns: 4})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(s.Close)
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("read test schema: %v", err)
	}
	if current != schema {
		t.Fatalf("search_path points at %q, want the test schema %q", current, schema)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate test store: %v", err)
	}
	return s, schema
}

func testNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func TestIntegrationMigrateIsIdempotent(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	before, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("nothing was applied")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	after, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("second Migrate applied %d more rows", len(after)-len(before))
	}
}

func TestIntegrationMigrateRefusesSchemaAhead(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `
INSERT INTO schema_migrations (version, name, checksum) VALUES (999, 'future', $1)`,
		make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	err := s.Migrate(ctx)
	var ahead *SchemaAheadError
	if !errors.As(err, &ahead) || ahead.Version != 999 {
		t.Fatalf("got %v, want SchemaAheadError for 999", err)
	}
}

func TestIntegrationMigrateRefusesChangedFile(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE schema_migrations SET checksum = $1 WHERE version = 1`,
		make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	err := s.Migrate(ctx)
	var changed *MigrationChecksumError
	if !errors.As(err, &changed) || changed.Version != 1 {
		t.Fatalf("got %v, want MigrationChecksumError for 1", err)
	}
}

func TestIntegrationMigrateSerializes(t *testing.T) {
	s, schema := integrationStore(t)
	ctx := context.Background()
	// A second store on the same schema applies nothing, and must not
	// return an error while the first holds the lock.
	other, err := Open(ctx, Config{URL: os.Getenv("MAGPIE_TEST_DATABASE_URL"), SearchPath: schema, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, st := range []*Store{s, other} {
		wg.Add(1)
		go func(i int, st *Store) {
			defer wg.Done()
			errs[i] = st.Migrate(ctx)
		}(i, st)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate %d: %v", i, err)
		}
	}
}

func TestIntegrationIdentity(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()

	first, err := s.EnsureVerifiedUser(ctx, EnsureVerifiedUserParams{
		Email: "User@Example.com", EmailNormalized: "user@example.com", VerifiedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RoleUser || first.Status != StatusActive || first.EmailVerifiedAt == nil || first.AuthVersion != 1 {
		t.Fatalf("first user: %+v", first)
	}
	second, err := s.EnsureVerifiedUser(ctx, EnsureVerifiedUserParams{
		Email: "user@example.com", EmailNormalized: "user@example.com", VerifiedAt: now.Add(time.Minute), PromoteAdmin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("a second sign-in made a second user")
	}
	if second.Role != RoleAdmin {
		t.Fatalf("admin list did not promote: %+v", second)
	}
	if second.EmailVerifiedAt == nil || !second.EmailVerifiedAt.Equal(*first.EmailVerifiedAt) {
		t.Fatal("a later sign-in moved email_verified_at")
	}

	got, err := s.UserByEmail(ctx, "user@example.com")
	if err != nil || got.ID != first.ID {
		t.Fatalf("UserByEmail: %+v %v", got, err)
	}
	if _, err := s.UserByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureVerifiedUser(ctx, EnsureVerifiedUserParams{
		Email: "user@example.com", EmailNormalized: "user@example.com", VerifiedAt: now,
	}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled user: got %v, want ErrUserDisabled", err)
	}
}

func TestIntegrationEmailCodeLifecycle(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	email := "code@example.com"
	ip := "203.0.113.9"
	hash1 := make([]byte, 32)
	hash1[0] = 1
	issue := func(hash []byte, at time.Time, expires time.Time) EmailCode {
		t.Helper()
		c, err := s.IssueEmailCode(ctx, IssueEmailCodeParams{
			EmailNormalized: email, Purpose: PurposeLoginForTest, CodeHash: hash,
			HashVersion: 1, MaxAttempts: 3, Now: at, ExpiresAt: expires, RequestIP: ip,
			Window: time.Hour, MaxSendsPerEmail: 100, MaxSendsPerIP: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c1 := issue(hash1, now, now.Add(10*time.Minute))
	live, err := s.LatestActiveEmailCode(ctx, email, PurposeLoginForTest, now)
	if err != nil || live.ID != c1.ID {
		t.Fatalf("latest: %+v %v", live, err)
	}
	a, err := s.RecordEmailCodeFailure(ctx, c1.ID, now)
	if err != nil || a.Attempts != 1 || a.Exhausted {
		t.Fatalf("first wrong code: %+v %v", a, err)
	}
	if _, err := s.RecordEmailCodeFailure(ctx, c1.ID, now); err != nil {
		t.Fatal(err)
	}
	a, err = s.RecordEmailCodeFailure(ctx, c1.ID, now)
	if err != nil || a.Attempts != 3 || !a.Exhausted {
		t.Fatalf("third wrong code: %+v %v", a, err)
	}
	if _, err := s.LatestActiveEmailCode(ctx, email, PurposeLoginForTest, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exhausted code still active: %v", err)
	}
	if err := s.ConsumeEmailCode(ctx, c1.ID, hash1, now); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("exhausted code consumed: %v", err)
	}

	hash2 := make([]byte, 32)
	hash2[0] = 2
	c2 := issue(hash2, now.Add(time.Second), now.Add(10*time.Minute))
	if err := s.ConsumeEmailCode(ctx, c2.ID, hash1, now); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("wrong hash consumed: %v", err)
	}
	if err := s.ConsumeEmailCode(ctx, c2.ID, hash2, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeEmailCode(ctx, c2.ID, hash2, now); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("second consume: %v", err)
	}

	c3 := issue(hash1, now.Add(2*time.Second), now.Add(3*time.Second))
	if _, err := s.LatestActiveEmailCode(ctx, email, PurposeLoginForTest, now.Add(4*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired code found: %v", err)
	}
	if err := s.ConsumeEmailCode(ctx, c3.ID, hash1, now.Add(4*time.Second)); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("expired code consumed: %v", err)
	}
}

func TestIntegrationSendThrottle(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	hash := make([]byte, 32)
	issue := func(email string, at time.Time, ip string, interval time.Duration, perEmail, perIP int) (EmailCode, error) {
		return s.IssueEmailCode(ctx, IssueEmailCodeParams{
			EmailNormalized: email, Purpose: PurposeLoginForTest, CodeHash: hash,
			HashVersion: 1, MaxAttempts: 5, Now: at, ExpiresAt: at.Add(10 * time.Minute),
			RequestIP: ip, Window: time.Hour,
			SendInterval: interval, MaxSendsPerEmail: perEmail, MaxSendsPerIP: perIP,
		})
	}

	first, err := issue("throttle@example.com", now, "203.0.113.9", time.Minute, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = issue("throttle@example.com", now.Add(10*time.Second), "203.0.113.9", time.Minute, 5, 10)
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("second send inside the interval: %v", err)
	}
	// The refused send must not have invalidated the live code.
	live, err := s.LatestActiveEmailCode(ctx, "throttle@example.com", PurposeLoginForTest, now.Add(10*time.Second))
	if err != nil || live.ID != first.ID {
		t.Fatalf("live code after refused send: %+v %v", live, err)
	}
	if _, err := issue("throttle@example.com", now.Add(61*time.Second), "203.0.113.9", time.Minute, 5, 10); err != nil {
		t.Fatalf("send after the interval: %v", err)
	}

	if _, err := issue("hourly@example.com", now, "198.51.100.1", 0, 1, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := issue("hourly@example.com", now.Add(time.Minute), "198.51.100.1", 0, 1, 10); !errors.As(err, &limited) {
		t.Fatalf("hourly cap: %v", err)
	}
	if _, err := issue("ip@example.com", now, "198.51.100.1", 0, 10, 1); !errors.As(err, &limited) {
		t.Fatalf("IP cap: %v", err)
	}
}

func TestIntegrationFailureCounts(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	hash := make([]byte, 32)
	c, err := s.IssueEmailCode(ctx, IssueEmailCodeParams{
		EmailNormalized: "counts@example.com", Purpose: PurposeLoginForTest, CodeHash: hash,
		HashVersion: 1, MaxAttempts: 5, Now: now, ExpiresAt: now.Add(10 * time.Minute),
		RequestIP: "203.0.113.9", Window: time.Hour, MaxSendsPerEmail: 10, MaxSendsPerIP: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.RecordEmailCodeFailure(ctx, c.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.EmailCodeFailureCounts(ctx, "counts@example.com", "203.0.113.9", now.Add(-time.Hour))
	if err != nil || counts.ByEmail != 2 || counts.ByIP != 2 {
		t.Fatalf("same IP: %+v %v", counts, err)
	}
	counts, err = s.EmailCodeFailureCounts(ctx, "counts@example.com", "198.51.100.1", now.Add(-time.Hour))
	if err != nil || counts.ByEmail != 2 || counts.ByIP != 0 {
		t.Fatalf("other IP: %+v %v", counts, err)
	}
	counts, err = s.EmailCodeFailureCounts(ctx, "counts@example.com", "203.0.113.9", now.Add(time.Minute))
	if err != nil || counts.ByEmail != 0 || counts.ByIP != 0 {
		t.Fatalf("outside the window: %+v %v", counts, err)
	}
}

func TestIntegrationSessions(t *testing.T) {
	s, _ := integrationStore(t)
	ctx := context.Background()
	now := testNow()
	user, err := s.EnsureVerifiedUser(ctx, EnsureVerifiedUserParams{
		Email: "session@example.com", EmailNormalized: "session@example.com", VerifiedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := make([]byte, 32)
	hash[0] = 7
	first, err := s.CreateSession(ctx, CreateSessionParams{
		UserID: user.ID, TokenHash: hash, AuthVersion: user.AuthVersion,
		Now: now, ExpiresAt: now.Add(time.Hour), UserAgent: "test", RequestIP: "203.0.113.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, gotUser, err := s.SessionByTokenHash(ctx, hash, now.Add(time.Minute))
	if err != nil || got.ID != first.ID || gotUser.ID != user.ID {
		t.Fatalf("session lookup: %+v %+v %v", got, gotUser, err)
	}
	if err := s.TouchSession(ctx, first.ID, now.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.SessionByTokenHash(ctx, hash, now.Add(10*time.Minute))
	if err != nil || got.LastSeenAt.Before(now.Add(10*time.Minute)) {
		t.Fatalf("last seen not touched: %+v %v", got, err)
	}
	if err := s.TouchSession(ctx, first.ID, now.Add(11*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSessionByTokenHash(ctx, hash, now.Add(12*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, hash, now.Add(12*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session found: %v", err)
	}

	hash2 := make([]byte, 32)
	hash2[0] = 8
	if _, err := s.CreateSession(ctx, CreateSessionParams{
		UserID: user.ID, TokenHash: hash2, AuthVersion: user.AuthVersion,
		Now: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	version, err := s.RevokeUserSessions(ctx, user.ID, now.Add(time.Minute))
	if err != nil || version != user.AuthVersion+1 {
		t.Fatalf("revoke all: %d %v", version, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, hash2, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived revoke-all: %v", err)
	}
	hash4 := make([]byte, 32)
	hash4[0] = 10
	if _, err := s.CreateSession(ctx, CreateSessionParams{
		UserID: user.ID, TokenHash: hash4, AuthVersion: user.AuthVersion,
		Now: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour),
	}); !errors.Is(err, ErrSessionStale) {
		t.Fatalf("old auth version accepted: %v", err)
	}
	if _, err := s.CreateSession(ctx, CreateSessionParams{
		UserID: user.ID, TokenHash: hash4, AuthVersion: version,
		Now: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("new auth version refused: %v", err)
	}

	hash3 := make([]byte, 32)
	hash3[0] = 9
	old := now.Add(-2 * time.Hour)
	if _, err := s.CreateSession(ctx, CreateSessionParams{
		UserID: user.ID, TokenHash: hash3, AuthVersion: version,
		Now: old, ExpiresAt: old.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, hash3, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session found: %v", err)
	}
}

// PurposeLoginForTest is the login purpose. It is a name rather than a
// literal so a typo in a test is a compile error.
const PurposeLoginForTest = "login"
