package access

import (
	"errors"
	"sync"
)

// CloudAuthenticator authenticates a database-backed cloud API key. The
// implementation must check status, expiry, ownership and channel grants on
// every call; a cached identity is not an authorization boundary.
type CloudAuthenticator interface {
	Authenticate(secret string) (Identity, error)
	Current(who Identity, secretSum [32]byte) bool
}

var cloudAuthenticator struct {
	sync.RWMutex
	a CloudAuthenticator
}

// RegisterCloudAuthenticator installs the cloud key store before the gateway
// starts. Local caller-keys.json is never used as a cloud fallback.
func RegisterCloudAuthenticator(a CloudAuthenticator) {
	cloudAuthenticator.Lock()
	defer cloudAuthenticator.Unlock()
	if a == nil {
		panic("access: nil cloud authenticator")
	}
	if cloudAuthenticator.a != nil {
		panic("access: cloud authenticator registered twice")
	}
	cloudAuthenticator.a = a
}

func cloudAuth() CloudAuthenticator {
	cloudAuthenticator.RLock()
	defer cloudAuthenticator.RUnlock()
	return cloudAuthenticator.a
}

var errCloudAuthUnavailable = errors.New("cloud API key authentication is unavailable")
