package gui

import (
	"strings"
	"testing"
)

func TestPaymentPEMEnvironmentAndCertificateBlocks(t *testing.T) {
	t.Setenv("TEST_PRIVATE_KEY", `line-one\nline-two`)
	if got := pemEnv("TEST_PRIVATE_KEY"); got != "line-one\nline-two" {
		t.Fatalf("%q", got)
	}
	certs := `-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\ndef\n-----END CERTIFICATE-----`
	blocks := certificateBlocks(certs)
	if len(blocks) != 2 || !strings.Contains(blocks[0], "abc") || !strings.Contains(blocks[1], "def") {
		t.Fatalf("%v", blocks)
	}
	for _, block := range blocks {
		if !strings.Contains(block, "-----BEGIN CERTIFICATE-----") || !strings.Contains(block, "-----END CERTIFICATE-----") {
			t.Fatalf("truncated certificate %q", block)
		}
	}
}
