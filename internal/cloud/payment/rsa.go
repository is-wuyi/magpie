package payment

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
)

const (
	// minRSAKeyBits is the smallest key both providers accept for RSA2 /
	// SHA256withRSA. SHA-1 and 1024-bit keys are refused, not downgraded.
	minRSAKeyBits = 2048
)

// parseRSAPrivateKeyPEM reads a PEM or bare-base64 RSA private key in PKCS#1
// or PKCS#8 form. An encrypted key is refused: the deployment passes plain
// key material from its secret store, never a password-protected file whose
// password would also have to be stored.
func parseRSAPrivateKeyPEM(text string) (*rsa.PrivateKey, error) {
	block, der, err := pemOrRawDER(text)
	if err != nil {
		return nil, err
	}
	if block != nil {
		if strings.Contains(strings.ToUpper(block.Headers["Proc-Type"]), "ENCRYPTED") {
			return nil, fmt.Errorf("%w: encrypted private keys are not supported", ErrInvalidConfig)
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: RSA private key: %v", ErrInvalidConfig, err)
			}
			return checkRSAPrivateKey(key)
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%w: PKCS#8 private key: %v", ErrInvalidConfig, err)
			}
			rk, ok := key.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("%w: private key is not RSA", ErrInvalidConfig)
			}
			return checkRSAPrivateKey(rk)
		}
	}
	// Unknown PEM type or bare base64: try both encodings, then refuse.
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return checkRSAPrivateKey(key)
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if rk, ok := key.(*rsa.PrivateKey); ok {
			return checkRSAPrivateKey(rk)
		}
	}
	return nil, fmt.Errorf("%w: private key is not a usable PKCS#1 or PKCS#8 RSA key", ErrInvalidConfig)
}

// parseRSAPublicKeyPEM reads a PEM or bare-base64 RSA public key in PKIX,
// PKCS#1 or X.509 certificate form. Alipay publishes its public key in the
// first two forms depending on the console; either is accepted.
func parseRSAPublicKeyPEM(text string) (*rsa.PublicKey, error) {
	block, der, err := pemOrRawDER(text)
	if err != nil {
		return nil, err
	}
	if block != nil {
		switch block.Type {
		case "PUBLIC KEY":
			return parsePKIXPublicKey(block.Bytes)
		case "RSA PUBLIC KEY":
			return parsePKCS1PublicKey(block.Bytes)
		case "CERTIFICATE":
			return publicKeyFromCertificate(block.Bytes)
		}
	}
	if key, err := parsePKIXPublicKey(der); err == nil {
		return key, nil
	}
	if key, err := parsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	if key, err := publicKeyFromCertificate(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: public key is not a usable PKIX, PKCS#1 or certificate key", ErrInvalidConfig)
}

func pemOrRawDER(text string) (*pem.Block, []byte, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil, fmt.Errorf("%w: key is empty", ErrInvalidConfig)
	}
	if block, _ := pem.Decode([]byte(trimmed)); block != nil {
		return block, block.Bytes, nil
	}
	raw := strings.Join(strings.Fields(trimmed), "")
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// The decoder's error names no key material, but keep it short.
		return nil, nil, fmt.Errorf("%w: key is neither PEM nor base64", ErrInvalidConfig)
	}
	return nil, der, nil
}

func checkRSAPrivateKey(key *rsa.PrivateKey) (*rsa.PrivateKey, error) {
	if key == nil || key.N == nil {
		return nil, fmt.Errorf("%w: empty RSA private key", ErrInvalidConfig)
	}
	if key.N.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("%w: RSA key is %d bits, at least %d are required", ErrInvalidConfig, key.N.BitLen(), minRSAKeyBits)
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("%w: RSA private key: %v", ErrInvalidConfig, err)
	}
	return key, nil
}

func parsePKIXPublicKey(der []byte) (*rsa.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	rk, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: public key is not RSA", ErrInvalidConfig)
	}
	return checkRSAPublicKey(rk)
}

func parsePKCS1PublicKey(der []byte) (*rsa.PublicKey, error) {
	key, err := x509.ParsePKCS1PublicKey(der)
	if err != nil {
		return nil, err
	}
	return checkRSAPublicKey(key)
}

func publicKeyFromCertificate(der []byte) (*rsa.PublicKey, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	rk, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: certificate key is not RSA", ErrInvalidConfig)
	}
	return checkRSAPublicKey(rk)
}

func checkRSAPublicKey(key *rsa.PublicKey) (*rsa.PublicKey, error) {
	if key == nil || key.N == nil {
		return nil, fmt.Errorf("%w: empty RSA public key", ErrInvalidConfig)
	}
	if key.N.BitLen() < minRSAKeyBits {
		return nil, fmt.Errorf("%w: RSA public key is %d bits, at least %d are required", ErrInvalidConfig, key.N.BitLen(), minRSAKeyBits)
	}
	return key, nil
}

// signSHA256RSA signs message with RSASSA-PKCS1-v1_5 and SHA-256 (Alipay's
// RSA2 and WeChat's WECHATPAY2-SHA256-RSA2048), returning base64.
func signSHA256RSA(key *rsa.PrivateKey, message string) (string, error) {
	digest := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("payment: sign message: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// verifySHA256RSA verifies a base64 RSASSA-PKCS1-v1_5 SHA-256 signature. A
// malformed signature is ErrInvalidSignature, never a fallback.
func verifySHA256RSA(pub *rsa.PublicKey, message, signatureBase64 string) error {
	if pub == nil {
		return fmt.Errorf("%w: no public key", ErrInvalidSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(signatureBase64)
	if err != nil || len(sig) == 0 {
		return fmt.Errorf("%w: signature is not base64", ErrInvalidSignature)
	}
	digest := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return fmt.Errorf("%w: signature does not verify", ErrInvalidSignature)
	}
	return nil
}

// sha256Hex is the audit digest stored with an event; it is never reversible
// back to the callback body.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
