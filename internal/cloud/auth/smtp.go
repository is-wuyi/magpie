package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrSMTPNotConfigured means the deployment asked for mail without a host or
// a From address.
var ErrSMTPNotConfigured = errors.New("auth: SMTP is not configured")

// SMTPConfig is the mail transport. It is read from the environment so a
// deployment never commits a password; the Mailer interface leaves room for
// another transport in a test or an embedding.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string

	// From is the envelope sender, as an address or "Name <address>".
	From string
	// FromName overrides a display name; empty leaves what From carries.
	FromName string

	// TLSMode is "starttls" (the default), "tls" (implicit TLS, usually port
	// 465) or "none" (a local relay with no credentials).
	TLSMode string

	Timeout time.Duration
}

// SMTPConfigFromEnv reads MAGPIE_SMTP_HOST, _PORT, _USERNAME, _PASSWORD,
// _FROM, _FROM_NAME, _TLS and _TIMEOUT. A missing host or From address is
// ErrSMTPNotConfigured; a malformed value is an error.
func SMTPConfigFromEnv() (SMTPConfig, error) {
	cfg := SMTPConfig{
		Host:     envOr("MAGPIE_SMTP_HOST", ""),
		Username: envOr("MAGPIE_SMTP_USERNAME", ""),
		Password: os.Getenv("MAGPIE_SMTP_PASSWORD"), // never trimmed: spaces can be part of it
		From:     envOr("MAGPIE_SMTP_FROM", ""),
		FromName: envOr("MAGPIE_SMTP_FROM_NAME", ""),
		TLSMode:  strings.ToLower(envOr("MAGPIE_SMTP_TLS", "starttls")),
	}
	if cfg.Host == "" || cfg.From == "" {
		return SMTPConfig{}, ErrSMTPNotConfigured
	}
	rawPort := envOr("MAGPIE_SMTP_PORT", "")
	if rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil {
			return SMTPConfig{}, fmt.Errorf("%w: MAGPIE_SMTP_PORT must be an integer", ErrInvalidConfig)
		}
		cfg.Port = port
	}
	rawTimeout := envOr("MAGPIE_SMTP_TIMEOUT", "")
	if rawTimeout != "" {
		timeout, err := time.ParseDuration(rawTimeout)
		if err != nil {
			return SMTPConfig{}, fmt.Errorf("%w: MAGPIE_SMTP_TIMEOUT must be a duration such as 15s", ErrInvalidConfig)
		}
		cfg.Timeout = timeout
	}
	if err := cfg.Validate(); err != nil {
		return SMTPConfig{}, err
	}
	return cfg.withDefaults(), nil
}

// withDefaults fills the port and timeout. It does not validate.
func (cfg SMTPConfig) withDefaults() SMTPConfig {
	cfg.TLSMode = strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if cfg.TLSMode == "" {
		cfg.TLSMode = "starttls"
	}
	if cfg.Port == 0 {
		switch cfg.TLSMode {
		case "tls":
			cfg.Port = 465
		case "none":
			cfg.Port = 25
		default:
			cfg.Port = 587
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	return cfg
}

// Validate refuses an SMTP configuration that would send a password in the
// clear or silently skip encryption.
func (cfg SMTPConfig) Validate() error {
	if strings.TrimSpace(cfg.Host) == "" || strings.ContainsAny(cfg.Host, " \t\r\n") {
		return fmt.Errorf("%w: SMTP host is missing or not usable", ErrInvalidConfig)
	}
	cfg = cfg.withDefaults()
	switch cfg.TLSMode {
	case "starttls", "tls", "none":
	default:
		return fmt.Errorf("%w: SMTP TLS mode must be starttls, tls or none", ErrInvalidConfig)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("%w: SMTP port is out of range", ErrInvalidConfig)
	}
	if cfg.TLSMode == "none" && (cfg.Username != "" || cfg.Password != "") {
		return fmt.Errorf("%w: SMTP credentials need TLS", ErrInvalidConfig)
	}
	if cfg.Username != "" && cfg.Password == "" {
		return fmt.Errorf("%w: SMTP username has no password", ErrInvalidConfig)
	}
	from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
	if err != nil || from.Address == "" {
		return fmt.Errorf("%w: SMTP From is not an address", ErrInvalidConfig)
	}
	if strings.ContainsAny(cfg.FromName, "\r\n") {
		return fmt.Errorf("%w: SMTP From name contains a line break", ErrInvalidConfig)
	}
	if cfg.Timeout < 0 {
		return fmt.Errorf("%w: SMTP timeout must not be negative", ErrInvalidConfig)
	}
	return nil
}

// SMTPSender delivers codes over SMTP. It holds no per-message state and is
// safe for concurrent use.
type SMTPSender struct {
	cfg  SMTPConfig
	from mail.Address
}

var _ Mailer = (*SMTPSender)(nil)

// NewSMTPSender validates cfg and prepares the envelope sender.
func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
	if err != nil {
		return nil, fmt.Errorf("%w: SMTP From is not an address", ErrInvalidConfig)
	}
	if cfg.FromName != "" {
		from.Name = cfg.FromName
	}
	return &SMTPSender{cfg: cfg, from: *from}, nil
}

// SendLoginCode sends one code. The message is built from fixed headers and
// validated addresses, so a code or an address cannot inject a header.
func (s *SMTPSender) SendLoginCode(ctx context.Context, msg LoginCodeMessage) error {
	to, err := NormalizeEmail(msg.To)
	if err != nil {
		return err
	}
	if !isSixDigits(msg.Code) {
		return errors.New("auth: a sign-in code must be six digits")
	}
	raw := buildLoginCodeMessage(s.from, to, msg, time.Now().UTC())
	return s.send(ctx, to, raw)
}

func (s *SMTPSender) send(ctx context.Context, to string, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("auth: SMTP dial: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return fmt.Errorf("auth: SMTP deadline: %w", err)
		}
	}
	if s.cfg.TLSMode == "tls" {
		tlsConn := tls.Client(conn, s.tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("auth: SMTP TLS handshake: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("auth: SMTP greeting: %w", err)
	}
	defer func() { _ = client.Close() }()

	if s.cfg.TLSMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("auth: SMTP server does not offer STARTTLS")
		}
		if err := client.StartTLS(s.tlsConfig()); err != nil {
			return fmt.Errorf("auth: SMTP STARTTLS: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("auth: SMTP server does not offer AUTH")
		}
		if err := client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("auth: SMTP auth: %w", err)
		}
	}
	if err := client.Mail(s.from.Address); err != nil {
		return fmt.Errorf("auth: SMTP sender: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("auth: SMTP recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("auth: SMTP data: %w", err)
	}
	if _, err := writer.Write(raw); err != nil {
		_ = writer.Close()
		return fmt.Errorf("auth: SMTP write: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("auth: SMTP data end: %w", err)
	}
	// The server's 250 answer to the data's end is the acceptance. A failed
	// QUIT after that does not make the mail unsent, and reporting it would
	// make the service discard a code the user already has.
	_ = client.Quit()
	return nil
}

func (s *SMTPSender) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
}

// buildLoginCodeMessage returns an RFC 5322 text/plain message with CRLF
// line endings. It carries the code in the body only.
func buildLoginCodeMessage(from mail.Address, to string, msg LoginCodeMessage, now time.Time) []byte {
	subject, intro := "Your magpie sign-in code", "Your sign-in code is:"
	switch msg.Purpose {
	case PurposeRegister:
		subject, intro = "Your magpie registration code", "Your registration code is:"
	case PurposeReset:
		subject, intro = "Your magpie password-reset code", "Your password-reset code is:"
	}
	var b strings.Builder
	b.WriteString("From: " + from.String() + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(intro + "\r\n\r\n")
	b.WriteString("    " + msg.Code + "\r\n\r\n")
	if remaining := msg.ExpiresAt.Sub(now); remaining > 0 {
		when := remaining.Round(time.Minute).String()
		if remaining >= time.Minute {
			when = fmt.Sprintf("%d minutes", int(remaining.Round(time.Minute)/time.Minute))
		}
		b.WriteString("It expires in " + when + " (at " +
			msg.ExpiresAt.UTC().Format("2006-01-02 15:04 MST") + ").\r\n\r\n")
	} else {
		b.WriteString("It expires at " + msg.ExpiresAt.UTC().Format("2006-01-02 15:04 MST") + ".\r\n\r\n")
	}
	b.WriteString("If you did not ask for this code, you can ignore this email. Nobody can sign in without it.\r\n")
	return []byte(b.String())
}
