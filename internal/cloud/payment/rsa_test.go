package payment

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// Alipay consoles hand out PKCS#1 PEM, PKCS#8 PEM and bare base64; all three
// must load.
func TestRSAKeyForms(t *testing.T) {
	keys := keysForTest(t)
	key := keys.alipayApp

	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pkcs8 := privateKeyPEM(t, key)
	publicPKIX := publicKeyPEM(t, &key.PublicKey)
	publicPKCS1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	barePrivate := base64.StdEncoding.EncodeToString(pkcs8DER)
	barePublic := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey))

	for name, text := range map[string]string{
		"pkcs1 private":       string(pkcs1),
		"pkcs8 private":       pkcs8,
		"bare base64 private": barePrivate,
	} {
		got, err := parseRSAPrivateKeyPEM(text)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.N.Cmp(key.N) != 0 {
			t.Errorf("%s: parsed another key", name)
		}
	}
	for name, text := range map[string]string{
		"pkix public":        publicPKIX,
		"pkcs1 public":       string(publicPKCS1),
		"bare base64 public": barePublic,
	} {
		got, err := parseRSAPublicKeyPEM(text)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.N.Cmp(key.N) != 0 {
			t.Errorf("%s: parsed another key", name)
		}
	}
}

func TestRSAKeyParsingRefusesWeakAndMalformedKeys(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Skipf("this Go refuses a 1024-bit test key: %v", err)
	}
	weakPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)})
	if _, err := parseRSAPrivateKeyPEM(string(weakPEM)); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("1024-bit private key error = %v, want ErrInvalidConfig", err)
	}
	weakPub := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&weak.PublicKey)})
	if _, err := parseRSAPublicKeyPEM(string(weakPub)); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("1024-bit public key error = %v, want ErrInvalidConfig", err)
	}

	encrypted := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
		Bytes:   []byte("not-a-key"),
	})
	if _, err := parseRSAPrivateKeyPEM(string(encrypted)); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("encrypted key error = %v, want ErrInvalidConfig", err)
	}
	for _, bad := range []string{"", "   ", "not base64 !!", strings.Repeat("A", 100)} {
		if _, err := parseRSAPrivateKeyPEM(bad); err == nil {
			t.Errorf("private key %q was accepted", bad)
		}
		if _, err := parseRSAPublicKeyPEM(bad); err == nil {
			t.Errorf("public key %q was accepted", bad)
		}
	}
}

func TestNewAlipayRefusesBadConfig(t *testing.T) {
	keys := keysForTest(t)
	goodKey := privateKeyPEM(t, keys.alipayApp)
	goodPub := publicKeyPEM(t, &keys.alipayApp.PublicKey)
	base := AlipayConfig{
		AppID:              testAlipayAppID,
		PrivateKeyPEM:      goodKey,
		AlipayPublicKeyPEM: goodPub,
		NotifyURL:          "https://shop.example.test/notify",
	}
	cases := map[string]func(*AlipayConfig){
		"no app id":       func(c *AlipayConfig) { c.AppID = "" },
		"no private key":  func(c *AlipayConfig) { c.PrivateKeyPEM = "" },
		"no public key":   func(c *AlipayConfig) { c.AlipayPublicKeyPEM = "" },
		"http gateway":    func(c *AlipayConfig) { c.GatewayURL = "http://openapi.alipay.com/gateway.do" },
		"no notify url":   func(c *AlipayConfig) { c.NotifyURL = "" },
		"http notify url": func(c *AlipayConfig) { c.NotifyURL = "http://shop.example.test/notify" },
		"http return url": func(c *AlipayConfig) { c.ReturnURL = "http://shop.example.test/return" },
	}
	for name, change := range cases {
		cfg := base
		change(&cfg)
		if _, err := NewAlipay(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: error = %v, want ErrInvalidConfig", name, err)
		}
	}
}

func TestNewWeChatRefusesBadConfig(t *testing.T) {
	keys := keysForTest(t)
	certPEM, _ := platformCertPEM(t, keys.wechatPlatform, testWeChatSerial)
	base := WeChatConfig{
		AppID:                testWeChatAppID,
		MchID:                testWeChatMchID,
		MerchantSerialNo:     testWeChatMerchantSN,
		PrivateKeyPEM:        privateKeyPEM(t, keys.wechatMerchant),
		APIv3Key:             testWeChatAPIv3Key,
		NotifyURL:            "https://shop.example.test/notify",
		PlatformCertificates: []string{certPEM},
	}
	cases := map[string]func(*WeChatConfig){
		"no app id":     func(c *WeChatConfig) { c.AppID = "" },
		"no mch id":     func(c *WeChatConfig) { c.MchID = "" },
		"no serial":     func(c *WeChatConfig) { c.MerchantSerialNo = "" },
		"short api key": func(c *WeChatConfig) { c.APIv3Key = "short" },
		"no certs":      func(c *WeChatConfig) { c.PlatformCertificates = nil },
		"bad cert":      func(c *WeChatConfig) { c.PlatformCertificates = []string{"not a cert"} },
		"http gateway":  func(c *WeChatConfig) { c.GatewayURL = "http://api.mch.weixin.qq.com" },
		"no notify url": func(c *WeChatConfig) { c.NotifyURL = "" },
		"http notify":   func(c *WeChatConfig) { c.NotifyURL = "http://shop.example.test/notify" },
		"huge skew":     func(c *WeChatConfig) { c.CallbackSkew = 2 * time.Hour },
		"negative skew": func(c *WeChatConfig) { c.CallbackSkew = -time.Second },
	}
	for name, change := range cases {
		cfg := base
		change(&cfg)
		if _, err := NewWeChat(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: error = %v, want ErrInvalidConfig", name, err)
		}
	}
}
