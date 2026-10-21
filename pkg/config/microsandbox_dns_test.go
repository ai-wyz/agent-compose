package config

import (
	"io"
	"log/slog"
	"testing"
)

func testConfigLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMicrosandboxDNSRebindProtectionDefaultsOn(t *testing.T) {
	t.Setenv("MICROSANDBOX_DNS_REBIND_PROTECTION", "")
	if !loadMicrosandboxDNSRebindProtection(testConfigLogger()) {
		t.Fatal("loadMicrosandboxDNSRebindProtection() = false, want the secure default true")
	}
}

func TestMicrosandboxDNSRebindProtectionReadsTheEnvironment(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "true", want: true},
		{raw: "1", want: true},
		{raw: " TRUE ", want: true},
		{raw: "false", want: false},
		{raw: "0", want: false},
		{raw: " FALSE ", want: false},
	}
	for _, test := range tests {
		t.Setenv("MICROSANDBOX_DNS_REBIND_PROTECTION", test.raw)
		if got := loadMicrosandboxDNSRebindProtection(testConfigLogger()); got != test.want {
			t.Fatalf("loadMicrosandboxDNSRebindProtection(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

// TestMicrosandboxDNSRebindProtectionKeepsTheDefaultOnParseError pins that a
// typo cannot silently disable the protection.
func TestMicrosandboxDNSRebindProtectionKeepsTheDefaultOnParseError(t *testing.T) {
	t.Setenv("MICROSANDBOX_DNS_REBIND_PROTECTION", "sometimes")
	if !loadMicrosandboxDNSRebindProtection(testConfigLogger()) {
		t.Fatal("loadMicrosandboxDNSRebindProtection() = false on a parse error, want true")
	}
}
