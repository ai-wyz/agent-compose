package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samber/do/v2"

	"github.com/chaitin/agent-compose/pkg/capmatrix"
	appconfig "github.com/chaitin/agent-compose/pkg/config"
)

// guestABIManifestWithCodexWithoutStructuredOutput differs from the static
// declaration on purpose: codex does support structured_output statically, so a
// snapshot built from this manifest proves the manifest source was used.
const guestABIManifestWithCodexWithoutStructuredOutput = `{
	"guestAbi": 1,
	"providers": {
		"codex":    {"version": "0.146.0", "features": ["session_resume", "streaming"]},
		"claude":   {"version": "2.1.220", "features": ["structured_output", "session_resume", "streaming", "skill_injection"]},
		"opencode": {"version": "1.18.9", "features": ["session_resume", "streaming", "skill_injection"]},
		"pi":       {"version": "1.3.0", "features": ["session_resume", "streaming", "skill_injection"]},
		"dsh":      {"version": "0.2.0-rc.1", "features": ["session_resume", "streaming", "skill_injection"]}
	}
}`

// TestNewEngineCapabilitySnapshotFallsBackToStaticProviders asserts that every
// way a configured manifest can fail leaves the legacy static provider facts in
// place instead of failing startup or exposing a partial matrix.
func TestNewEngineCapabilitySnapshotFallsBackToStaticProviders(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "no manifest configured", path: func(*testing.T) string { return "" }},
		{name: "unreadable manifest", path: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "absent.json")
		}},
		{name: "malformed manifest", path: func(t *testing.T) string {
			return writeGuestABIManifest(t, `{"guestAbi":`)
		}},
		{name: "unsupported abi", path: func(t *testing.T) string {
			return writeGuestABIManifest(t, `{"guestAbi":2,"providers":{"codex":{"version":"1.0.0"}}}`)
		}},
		{name: "manifest missing a required provider", path: func(t *testing.T) string {
			return writeGuestABIManifest(t, `{"guestAbi":1,"providers":{"codex":{"version":"1.0.0"}}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			di := do.New()
			do.ProvideValue(di, slog.Default())
			do.ProvideValue(di, &appconfig.Config{GuestAbiManifestPath: tc.path(t)})

			snapshot, err := NewEngineCapabilitySnapshot(di)
			if err != nil {
				t.Fatalf("NewEngineCapabilitySnapshot() error = %v", err)
			}
			want, err := capmatrix.DeclaredProviders()
			if err != nil {
				t.Fatalf("DeclaredProviders() error = %v", err)
			}
			if got := snapshot.Providers(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Providers() = %+v, want the legacy static matrix %+v", got, want)
			}
		})
	}
}

func TestNewEngineCapabilitySnapshotUsesGuestABIManifest(t *testing.T) {
	di := do.New()
	do.ProvideValue(di, slog.Default())
	do.ProvideValue(di, &appconfig.Config{
		GuestAbiManifestPath: writeGuestABIManifest(t, guestABIManifestWithCodexWithoutStructuredOutput),
	})

	snapshot, err := NewEngineCapabilitySnapshot(di)
	if err != nil {
		t.Fatalf("NewEngineCapabilitySnapshot() error = %v", err)
	}
	codex, ok := snapshot.Provider("codex")
	if !ok {
		t.Fatal("snapshot has no codex provider")
	}
	if codex.Feature(capmatrix.FeatureStructuredOutput) {
		t.Error("codex structured_output = true, want false from the manifest")
	}
	if !codex.Feature(capmatrix.FeatureSessionResume) {
		t.Error("codex session_resume = false, want true from the manifest")
	}

	static, err := capmatrix.DeclaredProviders()
	if err != nil {
		t.Fatalf("DeclaredProviders() error = %v", err)
	}
	for _, provider := range static {
		if provider.Provider == "codex" && !provider.Feature(capmatrix.FeatureStructuredOutput) {
			t.Fatal("static codex declaration no longer supports structured_output; this test can no longer prove the manifest was used")
		}
	}
}

func writeGuestABIManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guest-abi.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write guest abi manifest: %v", err)
	}
	return path
}
