package channels

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestCipherRoundTripAndTamper(t *testing.T) {
	c, err := NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"key":"sk-secret","headers":{"Authorization":"Bearer token"}}`)
	sealed, err := c.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret")) || bytes.Contains(sealed, []byte("token")) {
		t.Fatal("ciphertext contains provider secrets")
	}
	got, err := c.Open(sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := c.Open(sealed); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
}

func TestCloudProviderRequiresPortableKey(t *testing.T) {
	for name, p := range map[string]provider.Provider{
		"no key":       {ID: "relay", Name: "Relay", Chat: "https://relay.example/v1"},
		"local vertex": {ID: "vertex", Name: "Vertex", Key: "k", Gemini: "https://vertex.example", Vertex: &provider.Vertex{Credentials: "/tmp/credentials.json"}},
		"plugin route": {ID: "relay", Name: "Relay", Key: "k", Chat: "https://relay.example/v1", Routing: "plugin/smart"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProvider(p); err == nil {
				t.Fatal("accepted a provider that depends on local or plugin state")
			}
		})
	}
	if err := validateProvider(provider.Provider{ID: "relay", Name: "Relay", Key: "sk-test", Chat: "https://relay.example/v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderViewContainsNoSecretValues(t *testing.T) {
	secret := strings.Repeat("s", 32)
	v := viewOf(provider.Provider{ID: "u-user-relay", Name: "Relay", Key: secret, Chat: "https://relay.example/v1",
		Keys: []provider.KeyAccount{{Name: "spare", Key: secret}}, Headers: map[string]string{"Authorization": secret}})
	if v.KeySet != true || len(v.Keys) != 2 {
		t.Fatalf("%+v", v)
	}
	for _, k := range v.Keys {
		if strings.Contains(k.Masked, secret) {
			t.Fatal("view returned the upstream key")
		}
	}
	if v.Chat == "" || len(v.Keys) == 0 {
		t.Fatal("view lost its public configuration")
	}
}
