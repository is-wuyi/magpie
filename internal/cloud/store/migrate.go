package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrationsFS holds the numbered SQL files. A migration is written once,
// embedded, and never edited after it has been applied: Migrate compares the
// checksum of every applied file and refuses a changed one.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	// migrationLockKey is the advisory lock Migrate takes so two processes
	// starting at once cannot both apply a migration. It is specific to
	// this application, and advisory locks live per database.
	migrationLockKey int64 = 0x6d6167706965 // "magpie"

	migrationRetry = 100 * time.Millisecond
)

// schemaMigrationsDDL creates the bookkeeping table itself. It is not a
// numbered migration, because Migrate needs it before it can read anything.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    bigint      PRIMARY KEY,
	name       text        NOT NULL,
	checksum   bytea       NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now(),
	CONSTRAINT schema_migrations_version_positive CHECK (version > 0),
	CONSTRAINT schema_migrations_checksum_length CHECK (octet_length(checksum) = 32)
)`

// AppliedMigration is one row of schema_migrations.
type AppliedMigration struct {
	Version   int64
	Name      string
	Checksum  [sha256.Size]byte
	AppliedAt time.Time
}

// migration is one embedded SQL file.
type migration struct {
	version  int64
	name     string
	checksum [sha256.Size]byte
	sql      string
}

// Migrate applies every embedded migration that is not applied yet, in
// version order, each in its own transaction, under a PostgreSQL advisory
// lock. It refuses a changed applied file, a missing earlier version, and a
// database that is ahead of this binary.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil || s.connConfig == nil {
		return ErrClosed
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := pgx.ConnectConfig(ctx, s.connConfig.Copy())
	if err != nil {
		return fmt.Errorf("store: migration connection: %w", err)
	}
	defer func() {
		// The connection's end releases the advisory lock even if the
		// explicit unlock below fails.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if err := s.lockMigrations(ctx, conn); err != nil {
		return err
	}
	defer func() {
		// Best effort: closing the connection releases the lock if this
		// cannot run.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}

	known := make(map[int64]migration, len(migrations))
	for _, m := range migrations {
		known[m.version] = m
	}
	appliedByVersion := make(map[int64]AppliedMigration, len(applied))
	for _, a := range applied {
		m, ok := known[a.Version]
		if !ok {
			return &SchemaAheadError{Version: a.Version, Name: a.Name}
		}
		if !bytes.Equal(a.Checksum[:], m.checksum[:]) {
			return &MigrationChecksumError{Version: m.version, Name: m.name}
		}
		appliedByVersion[a.Version] = a
	}

	for _, m := range migrations {
		if _, ok := appliedByVersion[m.version]; ok {
			continue
		}
		// A version is missing while a later one is applied: the table was
		// edited by hand, or a migration failed after another process
		// recorded a later one. Applying it out of order would be worse.
		for version := range appliedByVersion {
			if version > m.version {
				return &MigrationGapError{Missing: m.version, Applied: version}
			}
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin migration %03d_%s: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %03d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum[:]); err != nil {
		return fmt.Errorf("store: record migration %03d_%s: %w", m.version, m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit migration %03d_%s: %w", m.version, m.name, err)
	}
	return nil
}

// lockMigrations waits for the advisory lock, giving up after the configured
// timeout. It never returns with the lock held by a connection it will keep:
// Migrate's connection is closed when Migrate returns.
func (s *Store) lockMigrations(ctx context.Context, conn *pgx.Conn) error {
	timeout := s.migrateLockTimeout
	if timeout <= 0 {
		timeout = DefaultMigrateLockTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLockKey).Scan(&locked); err != nil {
			return fmt.Errorf("store: acquire migration lock: %w", err)
		}
		if locked {
			return nil
		}
		if !time.Now().Before(deadline) {
			return ErrMigrationLocked
		}
		timer := time.NewTimer(migrationRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func appliedMigrations(ctx context.Context, q querier) ([]AppliedMigration, error) {
	rows, err := q.Query(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		var checksum []byte
		if err := rows.Scan(&a.Version, &a.Name, &checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
		if len(checksum) != sha256.Size {
			return nil, fmt.Errorf("store: migration %03d_%s has a %d-byte checksum", a.Version, a.Name, len(checksum))
		}
		copy(a.Checksum[:], checksum)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return out, nil
}

// AppliedMigrations returns what schema_migrations holds, in version order.
// It is for health checks and tests; serving requests does not need it.
func (s *Store) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	if s == nil || s.pool == nil {
		return nil, ErrClosed
	}
	return appliedMigrations(ctx, s.pool)
}

// loadMigrations reads, parses and checks every embedded migration. The
// files are contiguous from 1 and strictly ordered; a duplicate or a gap is
// a build error, not something to tolerate at startup.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		b, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", entry.Name(), err)
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("store: migration %s is empty", entry.Name())
		}
		out = append(out, migration{
			version:  version,
			name:     name,
			checksum: sha256.Sum256(b),
			sql:      string(b),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("store: no embedded migrations")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		want := int64(i + 1)
		if m.version != want {
			return nil, fmt.Errorf("store: migrations must be numbered 1..N without gaps; expected %03d, found %03d", want, m.version)
		}
	}
	return out, nil
}

// parseMigrationName reads NNN_name.sql. The version must be zero-padded to
// at least three digits and the name is lower-case ASCII, digits and
// underscores, so a file cannot be picked up under two spellings.
func parseMigrationName(file string) (int64, string, error) {
	base, ok := strings.CutSuffix(file, ".sql")
	if !ok {
		return 0, "", fmt.Errorf("store: migration %q does not end in .sql", file)
	}
	number, name, ok := strings.Cut(base, "_")
	if !ok || number == "" || name == "" {
		return 0, "", fmt.Errorf("store: migration %q is not NNN_name.sql", file)
	}
	version, err := strconv.ParseInt(number, 10, 64)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("store: migration %q has no positive version", file)
	}
	if len(number) < 3 || number != fmt.Sprintf("%0*d", len(number), version) {
		return 0, "", fmt.Errorf("store: migration %q must zero-pad its version to at least three digits", file)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return 0, "", fmt.Errorf("store: migration %q has a name outside [a-z0-9_]", file)
		}
	}
	return version, name, nil
}
