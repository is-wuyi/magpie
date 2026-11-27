package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var identity *migration
	for i := range ms {
		if ms[i].version == 1 {
			identity = &ms[i]
		}
	}
	if identity == nil || identity.name != "identity" {
		t.Fatalf("no 001_identity migration in %d embedded files", len(ms))
	}
	if identity.checksum != sha256.Sum256([]byte(identity.sql)) {
		t.Fatal("checksum is not the SQL file's SHA-256")
	}
	for _, want := range []string{"CREATE TABLE users", "CREATE TABLE email_codes", "CREATE TABLE user_sessions", "email_codes_one_live"} {
		if !strings.Contains(identity.sql, want) {
			t.Errorf("migration does not contain %q", want)
		}
	}
	for i, m := range ms {
		if m.version != int64(i+1) {
			t.Errorf("migration %03d is out of order at %d", m.version, i)
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	cases := []struct {
		name    string
		want    int64
		label   string
		wantErr bool
	}{
		{name: "001_identity.sql", want: 1, label: "identity"},
		{name: "002_wallet.sql", want: 2, label: "wallet"},
		{name: "1000_many_digits.sql", want: 1000, label: "many_digits"},
		{name: "1_one.sql", wantErr: true},
		{name: "01_one.sql", wantErr: true},
		{name: "001_Upper.sql", wantErr: true},
		{name: "001_one.SQL", wantErr: true},
		{name: "001.sql", wantErr: true},
		{name: "000_zero.sql", wantErr: true},
		{name: "abc_one.sql", wantErr: true},
		{name: "001_a-b.sql", wantErr: true},
	}
	for _, c := range cases {
		v, label, err := parseMigrationName(c.name)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: want an error, got version %d name %q", c.name, v, label)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if v != c.want || label != c.label {
			t.Errorf("%s: got %d/%q, want %d/%q", c.name, v, label, c.want, c.label)
		}
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := Config{URL: "postgres://magpie:pw@db:5432/magpie?sslmode=require"}.withDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != DefaultMaxConns || cfg.ConnectTimeout != DefaultConnectTimeout ||
		cfg.MigrateLockTimeout != DefaultMigrateLockTimeout || cfg.ApplicationName != DefaultApplicationName {
		t.Fatalf("defaults not filled: %+v", cfg)
	}

	if err := (Config{}).Validate(); !errors.Is(err, ErrNoDatabaseURL) {
		t.Fatalf("empty URL: got %v, want ErrNoDatabaseURL", err)
	}
	bad := Config{URL: "postgres://db/magpie", MaxConns: 1, MinConns: 2}.withDefaults()
	if err := bad.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("MinConns > MaxConns: got %v", err)
	}
	bad = Config{URL: "postgres://db/magpie", SearchPath: "x\nSET ROLE admin"}.withDefaults()
	if err := bad.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("control character in SearchPath: got %v", err)
	}
	bad = Config{URL: "postgres://db/magpie", RuntimeParams: map[string]string{"x": "a\x00b"}}.withDefaults()
	if err := bad.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("control character in RuntimeParams: got %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("MAGPIE_DATABASE_URL", "")
	t.Setenv("MAGPIE_DATABASE_MAX_CONNS", "")
	t.Setenv("MAGPIE_DATABASE_MIN_CONNS", "")
	t.Setenv("MAGPIE_DATABASE_CONNECT_TIMEOUT", "")
	t.Setenv("MAGPIE_DATABASE_MIGRATE_LOCK_TIMEOUT", "")
	t.Setenv("MAGPIE_DATABASE_APPLICATION_NAME", "")
	t.Setenv("MAGPIE_DATABASE_SEARCH_PATH", "")
	if _, err := ConfigFromEnv(); !errors.Is(err, ErrNoDatabaseURL) {
		t.Fatalf("no URL: got %v", err)
	}

	t.Setenv("MAGPIE_DATABASE_URL", "postgres://db/magpie")
	t.Setenv("MAGPIE_DATABASE_MAX_CONNS", "7")
	t.Setenv("MAGPIE_DATABASE_CONNECT_TIMEOUT", "3s")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "postgres://db/magpie" || cfg.MaxConns != 7 || cfg.ConnectTimeout != 3*time.Second {
		t.Fatalf("got %+v", cfg)
	}
	if cfg.MigrateLockTimeout != DefaultMigrateLockTimeout || cfg.ApplicationName != DefaultApplicationName {
		t.Fatalf("defaults not filled: %+v", cfg)
	}

	t.Setenv("MAGPIE_DATABASE_URL", "postgres://db/magpie")
	t.Setenv("MAGPIE_DATABASE_CONNECT_TIMEOUT", "soon")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("a malformed duration was accepted")
	}
}

func TestOpenRejectsBadConfigBeforeDialing(t *testing.T) {
	if _, err := Open(context.Background(), Config{}); !errors.Is(err, ErrNoDatabaseURL) {
		t.Fatalf("got %v, want ErrNoDatabaseURL", err)
	}
	if _, err := Open(context.Background(), Config{URL: "://not-a-url"}); !errors.Is(err, ErrInvalidDatabaseURL) {
		t.Fatalf("got %v, want ErrInvalidDatabaseURL", err)
	}
}

func TestParseIP(t *testing.T) {
	if ip, err := parseIP(""); err != nil || ip.IsValid() {
		t.Fatalf("empty: %v %v", ip, err)
	}
	ip, err := parseIP("203.0.113.9")
	if err != nil || ip.String() != "203.0.113.9" {
		t.Fatalf("IPv4: %v %v", ip, err)
	}
	if _, err := parseIP("not an address"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("malformed: %v", err)
	}
	if _, err := parseIP("fe80::1%en0"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zoned IPv6: %v", err)
	}
}

func TestValidNormalizedEmail(t *testing.T) {
	good := []string{"a@b.co", "user@example.com", "first.last+tag@example.co.uk"}
	bad := []string{"", "a@b", "A@b.co", " a@b.co", "a@b.co ", "a@@b.co", "a b@c.co", "a@b..co", "a@.co"}
	for _, s := range good {
		if !validNormalizedEmail(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if validNormalizedEmail(s) {
			t.Errorf("%q should be refused", s)
		}
	}
}

func TestClosedStore(t *testing.T) {
	var s *Store
	if err := s.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil store Ping: %v", err)
	}
	if err := s.Migrate(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil store Migrate: %v", err)
	}
	if _, err := s.UserByID(context.Background(), uuid.Nil); !errors.Is(err, ErrClosed) {
		// uuid.UUID is [16]byte; this checks the closed guard runs before
		// validation.
		t.Fatalf("nil store UserByID: %v", err)
	}
}

func TestEmailLockKeyIsStableAndDistinct(t *testing.T) {
	if emailLockKey("a@example.com") != emailLockKey("a@example.com") {
		t.Fatal("lock key is not stable")
	}
	if emailLockKey("a@example.com") == emailLockKey("b@example.com") {
		t.Fatal("two addresses share a lock key")
	}
}

func TestMigrationChecksumCoversBytes(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(ms[0].sql))
	if !bytes.Equal(ms[0].checksum[:], want[:]) {
		t.Fatal("checksum mismatch")
	}
}
