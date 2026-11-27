package gateway

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/provider"
)

func TestCloudEgressRefusesPrivateAndLiteralIPAddresses(t *testing.T) {
	cloud.SetEnabled(true)
	t.Cleanup(func() { cloud.SetEnabled(false) })
	for _, address := range []string{"127.0.0.1:80", "10.0.0.2:443", "169.254.169.254:80", "[::1]:443", "not-an-address"} {
		if err := cloudEgressControl("", address, nil); err == nil {
			t.Errorf("%s was allowed", address)
		}
	}
	if err := cloudEgressControl("", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("public upstream refused: %v", err)
	}
	t.Setenv("MAGPIE_ALLOW_PRIVATE_UPSTREAMS", "true")
	if err := cloudEgressControl("", "10.0.0.2:443", nil); err != nil {
		t.Fatalf("operator-enabled VPC upstream refused: %v", err)
	}
}

func TestCloudReservationOutlivesStreamingTurn(t *testing.T) {
	billing := &fakeCloudBilling{}
	state := &cloudBillingState{billing: billing, prices: fakeCloudPrices{}}
	r := httptestRequestWithIdentity(access.Identity{Cloud: true, UserID: uuid.NewString()}, state)
	p := provider.Provider{ID: "h-channel-openai", HouseChannelID: uuid.NewString()}
	if err := reserveCloudAttempt(r, p, "gpt-test", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ttl := billing.reserve[0].TTL; ttl != 24*time.Hour {
		t.Fatalf("reservation TTL = %s, want 24h", ttl)
	}
}
