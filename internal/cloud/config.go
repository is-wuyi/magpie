// Package cloud holds process-wide cloud deployment settings. Tenant data
// lives in PostgreSQL; this package only identifies the runtime mode.
package cloud

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

var enabled atomic.Bool

func init() {
	on, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("MAGPIE_CLOUD")))
	enabled.Store(on)
}

// Enabled reports whether this process serves the multi-user cloud product.
// Cloud mode never treats a loopback request as an authenticated operator.
func Enabled() bool { return enabled.Load() }

// SetEnabled selects cloud mode during server startup and in isolated tests.
func SetEnabled(on bool) { enabled.Store(on) }

// DatabaseURL is the PostgreSQL connection string used by the cloud stores.
func DatabaseURL() string { return strings.TrimSpace(os.Getenv("MAGPIE_DATABASE_URL")) }

// AdminEmails is the explicit allow-list of addresses that may become
// administrators after completing email verification.
func AdminEmails() []string {
	var out []string
	seen := map[string]bool{}
	for email := range strings.SplitSeq(os.Getenv("MAGPIE_ADMIN_EMAILS"), ",") {
		email = strings.ToLower(strings.TrimSpace(email))
		if email != "" && !seen[email] {
			seen[email] = true
			out = append(out, email)
		}
	}
	return out
}
