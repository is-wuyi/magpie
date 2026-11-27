package auth

import (
	"errors"
	"fmt"
	"time"
)

// Errors callers compare with errors.Is. The HTTP handlers map them to
// statuses; a service caller can do the same. An error from storage or the
// mailer is always ErrUnavailable or ErrMailFailed, never one of the
// decisions below, so a failed read cannot be mistaken for a valid code.
var (
	// ErrInvalidEmail means the address is not usable.
	ErrInvalidEmail = errors.New("auth: invalid email address")

	// ErrInvalidCode means the code is wrong, expired, used, exhausted or
	// no longer current. It deliberately does not say which.
	ErrInvalidCode = errors.New("auth: invalid or expired code")

	// ErrTooManyRequests means a throttle refused the request. A
	// RateLimitError carries the wait.
	ErrTooManyRequests = errors.New("auth: too many requests")

	// ErrAccountDisabled means the address was proven but the account may
	// not sign in.
	ErrAccountDisabled = errors.New("auth: account is disabled")

	// ErrUnauthenticated means no live session backs the request.
	ErrUnauthenticated = errors.New("auth: not signed in")

	// ErrForbidden means a signed-in user lacks the role.
	ErrForbidden = errors.New("auth: forbidden")

	// ErrMailFailed means the code was made but could not be mailed. The
	// code has been discarded as far as storage allows.
	ErrMailFailed = errors.New("auth: could not send the sign-in code")

	// ErrSignInFailed means the code was accepted but a session could not
	// be completed (a concurrent revoke-all, for example). The caller should
	// ask for a new code.
	ErrSignInFailed = errors.New("auth: sign-in could not be completed")

	// ErrUnavailable means storage failed. Callers should fail closed and
	// answer a generic error, not sign the request in.
	ErrUnavailable = errors.New("auth: sign-in storage is unavailable")
)

// RateLimitError carries how long a caller should wait. It unwraps to
// ErrTooManyRequests.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("auth: too many requests, retry after %s", e.RetryAfter.Round(time.Second))
}

func (e *RateLimitError) Unwrap() error { return ErrTooManyRequests }
