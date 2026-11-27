package gateway

import (
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/netguard"
)

var errCloudPrivateUpstream = errors.New("private upstream addresses are refused in cloud mode")

// cloudEgressControl checks the address of the actual socket, after DNS and
// proxies have been resolved. This is the SSRF boundary for tenant-provided
// base URLs, not a string check that a redirect or DNS rebinding could evade.
func cloudEgressControl(_, address string, _ syscall.RawConn) error {
	if !cloud.Enabled() || privateUpstreamsAllowed() {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !netguard.Public(ap.Addr()) {
		return errCloudPrivateUpstream
	}
	return nil
}

func privateUpstreamsAllowed() bool {
	// This is a deployment-level escape hatch for an operator whose house
	// upstreams live in a VPC. It is never a tenant setting.
	on, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("MAGPIE_ALLOW_PRIVATE_UPSTREAMS")))
	return on
}
