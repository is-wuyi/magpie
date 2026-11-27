package auth

import (
	"context"
	"time"
)

// Mailer delivers a sign-in code. The SMTP sender in this package is the
// production implementation; tests use a fake, and a deployment that wants
// another transport implements this interface.
type Mailer interface {
	SendLoginCode(ctx context.Context, msg LoginCodeMessage) error
}

// LoginCodeMessage is one code to deliver. To is the normalized address.
// Code is the six-digit secret and must not be logged or stored.
type LoginCodeMessage struct {
	To        string
	Code      string
	Purpose   string
	ExpiresAt time.Time
}
