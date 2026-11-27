package auth

import (
	"context"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestSMTPConfigValidate(t *testing.T) {
	base := SMTPConfig{Host: "smtp.example.com", From: "magpie <no-reply@example.com>"}
	cfg := base.withDefaults()
	if cfg.Port != 587 || cfg.TLSMode != "starttls" || cfg.Timeout != 15*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tlsCfg := SMTPConfig{Host: "smtp.example.com", From: "no-reply@example.com", TLSMode: "tls"}.withDefaults()
	if tlsCfg.Port != 465 {
		t.Fatalf("tls port: %d", tlsCfg.Port)
	}
	plain := SMTPConfig{Host: "localhost", From: "no-reply@example.com", TLSMode: "none"}.withDefaults()
	if plain.Port != 25 {
		t.Fatalf("plain port: %d", plain.Port)
	}
	if err := plain.Validate(); err != nil {
		t.Fatal(err)
	}

	bad := []SMTPConfig{
		{Host: "", From: "a@example.com"},
		{Host: "smtp.example.com", From: ""},
		{Host: "smtp.example.com", From: "not-an-address"},
		{Host: "smtp.example.com", From: "a@example.com", Port: 70000},
		{Host: "smtp.example.com", From: "a@example.com", TLSMode: "ssltls"},
		{Host: "smtp.example.com", From: "a@example.com", TLSMode: "none", Username: "user", Password: "pw"},
		{Host: "smtp.example.com", From: "a@example.com", Username: "user"},
		{Host: "smtp.example.com", From: "a@example.com", FromName: "a\r\nBcc: b@example.com"},
		{Host: "smtp.example.com", From: "a@example.com", Timeout: -time.Second},
	}
	for _, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}

func TestSMTPConfigFromEnv(t *testing.T) {
	for _, name := range []string{
		"MAGPIE_SMTP_HOST", "MAGPIE_SMTP_PORT", "MAGPIE_SMTP_USERNAME", "MAGPIE_SMTP_PASSWORD",
		"MAGPIE_SMTP_FROM", "MAGPIE_SMTP_FROM_NAME", "MAGPIE_SMTP_TLS", "MAGPIE_SMTP_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
	if _, err := SMTPConfigFromEnv(); err != ErrSMTPNotConfigured {
		t.Fatalf("empty environment: %v", err)
	}
	t.Setenv("MAGPIE_SMTP_HOST", "smtp.example.com")
	if _, err := SMTPConfigFromEnv(); err != ErrSMTPNotConfigured {
		t.Fatalf("missing From: %v", err)
	}
	t.Setenv("MAGPIE_SMTP_FROM", "no-reply@example.com")
	t.Setenv("MAGPIE_SMTP_TLS", "tls")
	t.Setenv("MAGPIE_SMTP_TIMEOUT", "9s")
	cfg, err := SMTPConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 465 || cfg.TLSMode != "tls" || cfg.Timeout != 9*time.Second {
		t.Fatalf("got %+v", cfg)
	}
	t.Setenv("MAGPIE_SMTP_PORT", "not-a-port")
	if _, err := SMTPConfigFromEnv(); err == nil {
		t.Fatal("malformed port accepted")
	}
	t.Setenv("MAGPIE_SMTP_PORT", "")
	t.Setenv("MAGPIE_SMTP_TIMEOUT", "soon")
	if _, err := SMTPConfigFromEnv(); err == nil {
		t.Fatal("malformed timeout accepted")
	}
}

func TestBuildLoginCodeMessage(t *testing.T) {
	from := mail.Address{Name: "magpie", Address: "no-reply@example.com"}
	msg := LoginCodeMessage{
		To:        "user@example.com",
		Code:      "123456",
		Purpose:   PurposeLogin,
		ExpiresAt: testNowTime.Add(10 * time.Minute),
	}
	raw := string(buildLoginCodeMessage(from, msg.To, msg, testNowTime))
	for _, want := range []string{
		"From: " + from.String() + "\r\n",
		"To: user@example.com\r\n",
		"Subject: Your magpie sign-in code\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=\"utf-8\"\r\n",
		"\r\n    " + msg.Code + "\r\n",
		"expires in 10 minutes",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("message misses %q:\n%s", want, raw)
		}
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' && (i == 0 || raw[i-1] != '\r') {
			t.Fatalf("bare LF at %d", i)
		}
		if raw[i] == '\r' && (i+1 == len(raw) || raw[i+1] != '\n') {
			t.Fatalf("bare CR at %d", i)
		}
	}
}

func TestSMTPSenderValidatesBeforeDialing(t *testing.T) {
	sender, err := NewSMTPSender(SMTPConfig{
		Host: "127.0.0.1", Port: 1, From: "no-reply@example.com", TLSMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sender.SendLoginCode(ctx, LoginCodeMessage{To: "user@example.com", Code: "12345"}); err == nil {
		t.Fatal("a five-digit code was accepted")
	}
	if err := sender.SendLoginCode(ctx, LoginCodeMessage{To: "not-an-address", Code: "123456"}); err == nil {
		t.Fatal("a malformed recipient was accepted")
	}
}
