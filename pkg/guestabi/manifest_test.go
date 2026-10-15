package guestabi

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestParseValidatesManifest covers the decode and validation rules together,
// because Parse is the single entry point a caller uses before trusting a
// manifest.
func TestParseValidatesManifest(t *testing.T) {
	for _, tc := range []struct {
		name            string
		data            string
		wantErrContains string
		wantRawFeatures []string
	}{
		{
			name: "accepts a valid manifest",
			data: `{"guestAbi":1,"providers":{"codex":{"version":"0.146.0","features":["structured_output","streaming"]}}}`,
		},
		{
			name:            "rejects malformed json",
			data:            `{"guestAbi":`,
			wantErrContains: "parse guest abi manifest",
		},
		{
			name:            "rejects a missing guestAbi instead of defaulting it",
			data:            `{"providers":{"codex":{"version":"0.146.0"}}}`,
			wantErrContains: "guestAbi",
		},
		{
			name:            "rejects zero guestAbi",
			data:            `{"guestAbi":0,"providers":{"codex":{"version":"0.146.0"}}}`,
			wantErrContains: "guestAbi",
		},
		{
			name:            "rejects negative guestAbi",
			data:            `{"guestAbi":-1,"providers":{"codex":{"version":"0.146.0"}}}`,
			wantErrContains: "guestAbi",
		},
		{
			name:            "rejects no providers",
			data:            `{"guestAbi":1,"providers":{}}`,
			wantErrContains: "no providers",
		},
		{
			name:            "rejects an empty provider version",
			data:            `{"guestAbi":1,"providers":{"codex":{"version":"  "}}}`,
			wantErrContains: "empty version",
		},
		{
			name:            "preserves an unknown feature",
			data:            `{"guestAbi":1,"providers":{"codex":{"version":"0.146.0","features":["streaming","telepathy"]}}}`,
			wantRawFeatures: []string{"streaming", "telepathy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := Parse([]byte(tc.data))
			if tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("Parse() error = nil, want an error containing %q", tc.wantErrContains)
				}
				if !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("Parse() error = %q, want it to contain %q", err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if tc.wantRawFeatures != nil {
				got := manifest.Providers["codex"].Features
				if !reflect.DeepEqual(got, tc.wantRawFeatures) {
					t.Fatalf("features = %v, want the manifest's order %v", got, tc.wantRawFeatures)
				}
			}
		})
	}
}

// TestValidateAcceptsUnknownFeatures pins the deliberate leniency: this release
// decides which feature names it understands, so Validate must not reject a
// newer guest's vocabulary.
func TestValidateAcceptsUnknownFeatures(t *testing.T) {
	manifest := Manifest{
		GuestAbi: CurrentGuestAbi,
		Providers: map[string]Provider{
			"codex": {Version: "0.146.0", Features: []string{"telepathy", "streaming"}},
		},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for unknown feature names", err)
	}
}

func TestVerifyCompatible(t *testing.T) {
	for _, tc := range []struct {
		name    string
		abi     int
		wantErr bool
	}{
		{name: "matching abi", abi: CurrentGuestAbi},
		{name: "older abi", abi: CurrentGuestAbi - 1, wantErr: true},
		{name: "newer abi", abi: CurrentGuestAbi + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (Manifest{GuestAbi: tc.abi}).VerifyCompatible()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("VerifyCompatible() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyCompatible() error = nil, want an unsupported ABI error")
			}
			if !errors.Is(err, ErrUnsupportedGuestAbi) {
				t.Fatalf("VerifyCompatible() error = %v, want ErrUnsupportedGuestAbi", err)
			}
			for _, want := range []string{strconv.Itoa(tc.abi), strconv.Itoa(CurrentGuestAbi)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("VerifyCompatible() error = %q, want it to name %s", err, want)
				}
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	t.Run("loads a valid manifest", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "guest-abi.json")
		if err := os.WriteFile(path, []byte(`{"guestAbi":1,"providers":{"pi":{"version":"0.82.1"}}}`), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		manifest, err := LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}
		if manifest.GuestAbi != CurrentGuestAbi || manifest.Providers["pi"].Version != "0.82.1" {
			t.Fatalf("LoadFile() = %+v, want the manifest on disk", manifest)
		}
	})

	t.Run("reports a missing file", func(t *testing.T) {
		_, err := LoadFile(filepath.Join(t.TempDir(), "absent.json"))
		if err == nil {
			t.Fatal("LoadFile() error = nil, want a read error")
		}
		if !strings.Contains(err.Error(), "read guest abi manifest") {
			t.Fatalf("LoadFile() error = %q, want read context", err)
		}
	})
}
