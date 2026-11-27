package channels

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/cloud"
	"github.com/yetone/magpie/internal/cloud/pricing"
	"github.com/yetone/magpie/internal/cloud/store"
	"github.com/yetone/magpie/internal/provider"
)

func integrationChannels(t *testing.T) (*store.Store, *Service, *APIKeys) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MAGPIE_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set MAGPIE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	root, err := store.Open(ctx, store.Config{URL: dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "channels_test_" + hex.EncodeToString(random[:])
	if _, err := root.Pool().Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schema)); err != nil {
		t.Fatal(err)
	}
	root.Close()
	db, err := store.OpenMigrated(ctx, store.Config{URL: dsn, SearchPath: schema, MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		root, err := store.Open(context.Background(), store.Config{URL: dsn, MaxConns: 1})
		if err == nil {
			_, _ = root.Pool().Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA %s CASCADE`, schema))
			root.Close()
		}
	})
	return db, New(db.Pool(), mustTestCipher(t)), NewAPIKeys(db.Pool())
}

func mustTestCipher(t *testing.T) *Cipher {
	t.Helper()
	c, _ := NewCipher(make([]byte, 32))
	return c
}

func integrationUser(t *testing.T, db *store.Store, email string) store.User {
	t.Helper()
	u, err := db.EnsureVerifiedUser(context.Background(), store.EnsureVerifiedUserParams{Email: email, EmailNormalized: email, VerifiedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPostgresTenantProvidersHouseGrantsAndKeys(t *testing.T) {
	db, svc, keys := integrationChannels(t)
	ctx := context.Background()
	alice := integrationUser(t, db, "alice@example.com")
	bob := integrationUser(t, db, "bob@example.com")
	secret := "sk-upstream-never-public-7319"
	for _, u := range []store.User{alice, bob} {
		if _, err := svc.CreateUserProvider(ctx, u.ID, provider.Provider{ID: "openai", Name: "OpenAI", Key: secret, Chat: "https://openai.example/v1", Models: []string{"gpt-test"}}); err != nil {
			t.Fatal(err)
		}
	}
	aliceRows, err := svc.ListUserProviders(ctx, alice.ID)
	if err != nil || len(aliceRows) != 1 || !strings.HasPrefix(aliceRows[0].ID, "u-") {
		t.Fatalf("alice providers %+v, %v", aliceRows, err)
	}
	for _, row := range aliceRows {
		if row.KeySet != true || strings.Contains(fmt.Sprintf("%+v", row), secret) {
			t.Fatal("provider view exposed its secret")
		}
	}
	var ciphertext []byte
	if err := db.Pool().QueryRow(ctx, `SELECT config_ciphertext FROM provider_secrets LIMIT 1`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), secret) {
		t.Fatal("database stored provider plaintext")
	}
	house, err := svc.CreateHouseChannel(ctx, "House", "paid")
	if err != nil {
		t.Fatal(err)
	}
	hp, err := svc.CreateHouseProvider(ctx, house.ID, provider.Provider{ID: "openai", Name: "House OpenAI", Key: secret, Chat: "https://house.example/v1", Models: []string{"gpt-test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetHousePrice(ctx, house.ID, hp.ID, "gpt-test", pricing.Price{InputPerMTok: 100, OutputPerMTok: 200}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetHouseStatus(ctx, house.ID, "published"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Grant(ctx, alice.ID, house.ID); err != nil {
		t.Fatal(err)
	}
	group, err := svc.CreateUserGroup(ctx, alice.ID, provider.Group{ID: "mixed", Name: "Mixed", Members: []string{aliceRows[0].ID + "/gpt-test", hp.ID + "/gpt-test"}, Routing: provider.Ordered})
	if err != nil || group.OwnerUserID != alice.ID.String() {
		t.Fatalf("group %+v, %v", group, err)
	}
	if _, err := svc.CreateUserGroup(ctx, bob.ID, provider.Group{ID: "stolen", Name: "Stolen", Members: []string{aliceRows[0].ID + "/gpt-test"}}); err == nil {
		t.Fatal("Bob created a group over Alice's provider")
	}
	aliceGroups, err := svc.ListUserGroups(ctx, alice.ID)
	if err != nil || len(aliceGroups) != 1 {
		t.Fatalf("alice groups %+v, %v", aliceGroups, err)
	}
	bobGroups, err := svc.ListUserGroups(ctx, bob.ID)
	if err != nil || len(bobGroups) != 0 {
		t.Fatalf("bob sees Alice's groups: %+v, %v", bobGroups, err)
	}
	created, err := keys.Create(ctx, alice.ID, "laptop", nil, []string{house.ID.String()}, nil, 2)
	if err != nil || created.Secret == "" {
		t.Fatal(err)
	}
	who, err := keys.Authenticate(created.Secret)
	if err != nil || who.UserID != alice.ID.String() || len(who.HouseChannels) != 1 || who.MaxConcurrency != 2 {
		t.Fatalf("identity %+v, %v", who, err)
	}
	other, err := keys.Create(ctx, bob.ID, "other", nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherWho, err := keys.Authenticate(other.Secret)
	if err != nil || len(otherWho.HouseChannels) != 0 {
		t.Fatalf("ungranted identity %+v, %v", otherWho, err)
	}
	if err := svc.SetHouseStatus(ctx, house.ID, "offline"); err != nil {
		t.Fatal(err)
	}
	who, err = keys.Authenticate(created.Secret)
	if err != nil || len(who.HouseChannels) != 0 {
		t.Fatalf("offline channel stayed authorized: %+v, %v", who, err)
	}
	if err := keys.Revoke(ctx, alice.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(created.Secret); err == nil {
		t.Fatal("revoked key still authenticates")
	}
}

func TestPostgresProviderMutationsRefreshCatalog(t *testing.T) {
	db, svc, _ := integrationChannels(t)
	alice := integrationUser(t, db, "catalog@example.com")
	cloud.SetEnabled(true)
	t.Cleanup(func() {
		cloud.SetEnabled(false)
		provider.Changed()
	})
	provider.RegisterCloudLoader(svc.Load)
	provider.Changed()
	if got := provider.All(); len(got) != 0 {
		t.Fatalf("new schema started with providers: %+v", got)
	}
	created, err := svc.CreateUserProvider(context.Background(), alice.ID, provider.Provider{ID: "fresh", Name: "Fresh", Key: "secret", Chat: "https://fresh.example/v1", Models: []string{"m"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(created.ID); err != nil {
		t.Fatalf("a new provider was absent from the immediate catalog: %v", err)
	}
	if err := svc.DeleteUserProvider(context.Background(), alice.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(created.ID); err == nil {
		t.Fatal("a deleted provider stayed in the immediate catalog")
	}
}
