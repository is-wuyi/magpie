// Package store is the PostgreSQL source of truth for magpie's cloud mode.
//
// It owns the connection pool, the explicit numbered schema migrations, and
// the identity rows (users, one-time email codes and sessions) that the
// authentication service builds on. Every method fails closed: a database
// error is returned to the caller, never turned into an empty result, and
// Open refuses a store it cannot reach.
package store

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults for a cloud deployment. They are deliberately conservative and
// can be overridden through Config or the environment.
const (
	DefaultMaxConns           int32 = 10
	DefaultConnectTimeout           = 10 * time.Second
	DefaultMigrateLockTimeout       = time.Minute
	DefaultApplicationName          = "magpie-cloud"

	// envDatabaseURL is the only variable the store reads. A generic
	// DATABASE_URL is deliberately not used: another app's setting must
	// never silently point magpie at its database.
	envDatabaseURL = "MAGPIE_DATABASE_URL"
)

// Config is everything Open needs. The zero value is not usable: URL is
// required. Open fills the zero defaults.
type Config struct {
	// URL is a PostgreSQL connection URL, for example
	// postgres://magpie:secret@db:5432/magpie?sslmode=require.
	URL string

	// MaxConns and MinConns bound the pgx pool. Zero means the default for
	// MaxConns and no idle connections for MinConns.
	MaxConns int32
	MinConns int32

	// ConnectTimeout bounds the initial reachability check and each
	// connection attempt. Zero means DefaultConnectTimeout.
	ConnectTimeout time.Duration

	// MigrateLockTimeout bounds how long Migrate waits for another process
	// that is already applying migrations. Zero means
	// DefaultMigrateLockTimeout.
	MigrateLockTimeout time.Duration

	// ApplicationName is sent to PostgreSQL. Empty means
	// DefaultApplicationName.
	ApplicationName string

	// SearchPath, when set, is sent as PostgreSQL's search_path. The schema
	// must exist. It is for deployments that keep magpie's tables in a
	// dedicated schema, and for tests that use a throwaway schema. It must
	// not come from untrusted input.
	SearchPath string

	// RuntimeParams are extra PostgreSQL startup parameters. They must not
	// come from untrusted input.
	RuntimeParams map[string]string
}

// ConfigFromEnv reads MAGPIE_DATABASE_URL and the optional MAGPIE_DATABASE_*
// settings. A missing or malformed value is an error, never
// a silently disabled store.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		URL:             firstEnv(envDatabaseURL),
		ApplicationName: strings.TrimSpace(os.Getenv("MAGPIE_DATABASE_APPLICATION_NAME")),
		SearchPath:      strings.TrimSpace(os.Getenv("MAGPIE_DATABASE_SEARCH_PATH")),
	}
	var err error
	if cfg.MaxConns, err = envInt32("MAGPIE_DATABASE_MAX_CONNS", 0); err != nil {
		return Config{}, err
	}
	if cfg.MinConns, err = envInt32("MAGPIE_DATABASE_MIN_CONNS", 0); err != nil {
		return Config{}, err
	}
	if cfg.ConnectTimeout, err = envDuration("MAGPIE_DATABASE_CONNECT_TIMEOUT", 0); err != nil {
		return Config{}, err
	}
	if cfg.MigrateLockTimeout, err = envDuration("MAGPIE_DATABASE_MIGRATE_LOCK_TIMEOUT", 0); err != nil {
		return Config{}, err
	}
	if cfg.URL == "" {
		return Config{}, ErrNoDatabaseURL
	}
	return cfg.withDefaults(), nil
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func envInt32(name string, def int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an integer", ErrInvalidConfig, name)
	}
	return int32(n), nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a duration such as 10s or 2m", ErrInvalidConfig, name)
	}
	return d, nil
}

// withDefaults returns cfg with its unset values filled in. It does not
// validate.
func (cfg Config) withDefaults() Config {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = DefaultConnectTimeout
	}
	if cfg.MigrateLockTimeout == 0 {
		cfg.MigrateLockTimeout = DefaultMigrateLockTimeout
	}
	if cfg.ApplicationName == "" {
		cfg.ApplicationName = DefaultApplicationName
	}
	return cfg
}

// Validate refuses a configuration that could make the store behave in an
// unexpected way. It never contacts the database.
func (cfg Config) Validate() error {
	if strings.TrimSpace(cfg.URL) == "" {
		return ErrNoDatabaseURL
	}
	if cfg.MaxConns < 0 || cfg.MinConns < 0 {
		return fmt.Errorf("%w: connection counts must not be negative", ErrInvalidConfig)
	}
	maxConns := cfg.MaxConns
	if maxConns == 0 {
		maxConns = DefaultMaxConns
	}
	if cfg.MinConns > maxConns {
		return fmt.Errorf("%w: MinConns must not exceed MaxConns", ErrInvalidConfig)
	}
	if cfg.ConnectTimeout < 0 || cfg.MigrateLockTimeout < 0 {
		return fmt.Errorf("%w: timeouts must not be negative", ErrInvalidConfig)
	}
	if !validStartupValue(cfg.ApplicationName) {
		return fmt.Errorf("%w: ApplicationName contains a control character", ErrInvalidConfig)
	}
	if cfg.SearchPath != "" && !validStartupValue(cfg.SearchPath) {
		return fmt.Errorf("%w: SearchPath contains a control character", ErrInvalidConfig)
	}
	keys := make([]string, 0, len(cfg.RuntimeParams))
	for k := range cfg.RuntimeParams {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if k == "" || !validStartupValue(k) || !validStartupValue(cfg.RuntimeParams[k]) {
			return fmt.Errorf("%w: runtime parameter %q is not usable", ErrInvalidConfig, k)
		}
	}
	return nil
}

// validStartupValue reports whether s can be sent in a startup packet: no
// NUL and no line breaks, which PostgreSQL would reject anyway.
func validStartupValue(s string) bool {
	for _, r := range s {
		if r == 0 || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

// Store holds the pool every identity query runs on and the connection
// settings Migrate needs for its dedicated, locked connection.
type Store struct {
	pool               *pgxpool.Pool
	connConfig         *pgx.ConnConfig
	migrateLockTimeout time.Duration
}

// Pool shares the same bounded PostgreSQL pool with the cloud domain
// services. Close is owned by Store; domain services must not close it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Open creates a pool and checks that the database answers. It does not
// apply migrations; call Migrate (or OpenMigrated) before serving requests,
// because an unmigrated database makes every identity method fail.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx includes the raw URL, and sometimes its password, in parse
		// errors. Keep that out of logs and replies.
		return nil, ErrInvalidDatabaseURL
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = make(map[string]string, len(cfg.RuntimeParams)+2)
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName
	if cfg.SearchPath != "" {
		poolCfg.ConnConfig.RuntimeParams["search_path"] = cfg.SearchPath
	}
	for k, v := range cfg.RuntimeParams {
		poolCfg.ConnConfig.RuntimeParams[k] = v
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: create connection pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: database is not reachable: %w", err)
	}
	return &Store{
		pool:               pool,
		connConfig:         pool.Config().ConnConfig.Copy(),
		migrateLockTimeout: cfg.MigrateLockTimeout,
	}, nil
}

// OpenMigrated opens the pool and applies every pending migration. It is
// the usual startup call for a cloud deployment: either both steps work or
// no Store is returned.
func OpenMigrated(ctx context.Context, cfg Config) (*Store, error) {
	s, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases every pooled connection. It is safe to call more than once.
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Ping checks that the database still answers. A failed read is reported as
// an error, never as an empty store.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return ErrClosed
	}
	return s.pool.Ping(ctx)
}
