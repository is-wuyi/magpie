package gateway

import (
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/provider"
)

func TestCloudProviderACLCoversOwnHouseAndLegacy(t *testing.T) {
	cloud.SetEnabled(true)
	t.Cleanup(func() { cloud.SetEnabled(false) })
	who := access.Identity{Cloud: true, UserID: "user-a", HouseChannels: []string{"house-a"}}
	for name, tc := range map[string]struct {
		p    provider.Provider
		want bool
	}{
		"own":                {p: provider.Provider{OwnerUserID: "user-a"}, want: true},
		"other user":         {p: provider.Provider{OwnerUserID: "user-b"}},
		"granted house":      {p: provider.Provider{HouseChannelID: "house-a"}, want: true},
		"other house":        {p: provider.Provider{HouseChannelID: "house-b"}},
		"legacy local":       {p: provider.Provider{}},
		"forged both owners": {p: provider.Provider{OwnerUserID: "user-a", HouseChannelID: "house-a"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := providerAllowed(who, tc.p); got != tc.want {
				t.Fatalf("providerAllowed = %v, want %v", got, tc.want)
			}
		})
	}
	if providerAllowed(access.Identity{}, provider.Provider{OwnerUserID: "user-a"}) {
		t.Fatal("keyless cloud caller reached a tenant provider")
	}
}

func TestCloudGroupFiltersOtherTenantMembers(t *testing.T) {
	cloud.SetEnabled(true)
	t.Cleanup(func() { cloud.SetEnabled(false) })
	who := access.Identity{Cloud: true, UserID: "user-a"}
	g := provider.Group{Auto: true, Members: []string{"u-a/m", "u-b/m"}}
	ms := []provider.Member{{Provider: provider.Provider{ID: "u-a", OwnerUserID: "user-a"}, Model: "m"},
		{Provider: provider.Provider{ID: "u-b", OwnerUserID: "user-b"}, Model: "m"}}
	if !groupAllowed(who, g, ms) {
		t.Fatal("an auto group with one visible member was hidden")
	}
	if modelAllowed(who, ms[1].Provider, "m") {
		t.Fatal("another tenant's member remained a routing candidate")
	}
	if !groupAllowed(who, g, ms[:1]) {
		t.Fatal("own provider was filtered from its auto group")
	}
}
