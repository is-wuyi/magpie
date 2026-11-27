package store

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors callers compare with errors.Is. Each one means the exact
// condition its name says; a database failure is returned as its own error
// and is never translated into one of these.
var (
	// ErrNoDatabaseURL means Config.URL is empty.
	ErrNoDatabaseURL = errors.New("store: no database URL is configured")

	// ErrInvalidDatabaseURL means the URL could not be parsed. The raw URL
	// is deliberately not included, because it may carry a password.
	ErrInvalidDatabaseURL = errors.New("store: the database URL is not valid")

	// ErrInvalidConfig means a Config value cannot be used.
	ErrInvalidConfig = errors.New("store: invalid configuration")

	// ErrInvalidArgument means a query was given a value that would make
	// it unsafe or ambiguous to run.
	ErrInvalidArgument = errors.New("store: invalid argument")

	// ErrNotFound means the row does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrClosed means the Store has been closed.
	ErrClosed = errors.New("store: closed")

	// ErrUserDisabled means a user exists but must not be signed in.
	ErrUserDisabled = errors.New("store: user is disabled")

	// ErrCodeUnavailable means an email code is consumed, expired, or has
	// no attempts left.
	ErrCodeUnavailable = errors.New("store: email code is unavailable")

	// ErrSessionStale means the session cannot be created: the user is
	// missing, disabled, unverified, or has a newer auth version.
	ErrSessionStale = errors.New("store: session is stale")

	// ErrRateLimited means a send or verification is inside its throttle
	// window.
	ErrRateLimited = errors.New("store: rate limited")

	// ErrMigrationLocked means another process held the migration lock for
	// longer than Config.MigrateLockTimeout.
	ErrMigrationLocked = errors.New("store: the migration lock is held by another process")
)

// RateLimitError carries how long the caller should wait. It unwraps to
// ErrRateLimited.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("store: rate limited for %s", e.RetryAfter.Round(time.Second))
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// MigrationChecksumError means an applied migration file differs from the
// embedded one. Rewriting an applied migration is refused: add a new one.
type MigrationChecksumError struct {
	Version int64
	Name    string
}

func (e *MigrationChecksumError) Error() string {
	return fmt.Sprintf("store: migration %03d_%s changed after it was applied", e.Version, e.Name)
}

// SchemaAheadError means the database has a migration this binary does not
// know. Running an older binary against a newer schema is refused.
type SchemaAheadError struct {
	Version int64
	Name    string
}

func (e *SchemaAheadError) Error() string {
	return fmt.Sprintf("store: database schema is ahead of this binary (migration %03d_%s)", e.Version, e.Name)
}

// MigrationGapError means a later migration is applied while an earlier one
// is not. Migrations are explicit and contiguous, so this is a damaged or
// hand-edited schema_migrations table.
type MigrationGapError struct {
	Missing int64
	Applied int64
}

func (e *MigrationGapError) Error() string {
	return fmt.Sprintf("store: migration %03d is missing while %03d is applied", e.Missing, e.Applied)
}
