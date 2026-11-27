package channels

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Cipher encrypts complete provider configurations. Callers never need to
// enumerate every credential field, so a newly added provider option cannot
// accidentally become plaintext metadata.
type Cipher struct{ aead cipher.AEAD }

func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cloud configuration key must be 32 bytes, got %d", len(key))
	}
	a, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: a}, nil
}

func CipherFromEnv() (*Cipher, error) {
	raw := strings.TrimSpace(os.Getenv("MAGPIE_CONFIG_KEY"))
	if raw == "" {
		return nil, errors.New("MAGPIE_CONFIG_KEY is required in cloud mode")
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return NewCipher(b)
	}
	if b, err := hex.DecodeString(raw); err == nil {
		return NewCipher(b)
	}
	return nil, errors.New("MAGPIE_CONFIG_KEY must be base64 or hex encoded")
}

func mustAES(key []byte) cipher.Block {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // NewCipher checked the only possible failure.
	}
	return b
}

func (c *Cipher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, []byte("magpie.cloud.provider.v1")), nil
}

func (c *Cipher) Open(packed []byte) ([]byte, error) {
	if len(packed) < c.aead.NonceSize() {
		return nil, errors.New("encrypted provider configuration is truncated")
	}
	nonce := packed[:c.aead.NonceSize()]
	return c.aead.Open(nil, nonce, packed[c.aead.NonceSize():], []byte("magpie.cloud.provider.v1"))
}
