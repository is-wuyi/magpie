package auth

import (
	"fmt"
	"net/mail"
	"strings"
)

// NormalizeEmail turns a typed address into the key every lookup uses:
// trimmed, lower-cased and without a display name. It rejects anything
// net/mail cannot parse as a bare mailbox, so a code can never be sent to a
// header-injection shape.
func NormalizeEmail(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 254 {
		return "", fmt.Errorf("%w: address is empty or too long", ErrInvalidEmail)
	}
	for _, r := range s {
		if r == 0 || r == '\r' || r == '\n' {
			return "", fmt.Errorf("%w: address contains a control character", ErrInvalidEmail)
		}
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return "", fmt.Errorf("%w: address is not a bare mailbox", ErrInvalidEmail)
	}
	normalized := strings.ToLower(addr.Address)
	at := strings.IndexByte(normalized, '@')
	if at <= 0 || at == len(normalized)-1 {
		return "", fmt.Errorf("%w: address has no mailbox", ErrInvalidEmail)
	}
	local, domain := normalized[:at], normalized[at+1:]
	if strings.ContainsAny(local, " \t") || strings.ContainsAny(domain, " \t") {
		return "", fmt.Errorf("%w: address contains whitespace", ErrInvalidEmail)
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", fmt.Errorf("%w: address domain is not usable", ErrInvalidEmail)
	}
	if len(normalized) > 254 {
		return "", fmt.Errorf("%w: address is too long", ErrInvalidEmail)
	}
	return normalized, nil
}
