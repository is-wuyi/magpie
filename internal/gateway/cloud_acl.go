package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/provider"
)

// cloudRouteGuard removes the legacy owner, subscription and local-resource
// API plane. Standard inference protocols keep their existing paths; magpie's
// own API is reduced to self-service data that is explicitly tenant-scoped.
func cloudRouteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cloud.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		blocked := p == JoinPagePath || p == ChatPagePath || strings.HasPrefix(p, "/mcp/") || strings.HasPrefix(p, "/_magpie/") || p == "/mcp" || p == "/_magpie"
		if strings.HasPrefix(p, "/v1/magpie/") {
			switch p {
			case "/v1/magpie/limit", "/v1/magpie/me", "/v1/magpie/usage/requests", "/v1/magpie/quotas", "/v1/magpie/quotas/history":
			default:
				blocked = true
			}
		}
		if blocked {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// providerAllowed is the tenant boundary for both model discovery and every
// routing candidate. Local providers have no cloud owner and are therefore
// never exposed by a cloud process.
func providerAllowed(who access.Identity, p provider.Provider) bool {
	if !cloud.Enabled() {
		return true
	}
	if !who.Cloud || who.UserID == "" {
		return false
	}
	if p.OwnerUserID != "" {
		return p.HouseChannelID == "" && p.OwnerUserID == who.UserID
	}
	return p.HouseChannelID != "" && slices.Contains(who.HouseChannels, p.HouseChannelID)
}

func groupCloudAllowed(who access.Identity, g provider.Group) bool {
	if cloud.Enabled() && g.Auto {
		return who.Cloud && who.UserID != ""
	}
	return providerAllowed(who, provider.Provider{OwnerUserID: g.OwnerUserID, HouseChannelID: g.HouseChannelID})
}

func membersCloudAllowed(who access.Identity, ms []provider.Member) bool {
	if !cloud.Enabled() {
		return true
	}
	if len(ms) == 0 {
		return false
	}
	// A group may be used only through a visible member. Every candidate is
	// filtered again by modelAllowed, so another tenant's member is never a
	// route; one mixed auto group need not disappear for both tenants.
	for _, m := range ms {
		if providerAllowed(who, m.Provider) {
			return true
		}
	}
	return false
}
