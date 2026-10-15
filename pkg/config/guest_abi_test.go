package config

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/samber/do/v2"
)

func TestNewConfigGuestABIManifestPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "defaults to empty", raw: "", want: ""},
		{name: "trims whitespace", raw: "  /opt/guest/guest-abi.json \n", want: "/opt/guest/guest-abi.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DATA_ROOT", filepath.Join(root, "data"))
			t.Setenv(guestABIManifestEnv, tc.raw)

			di := do.New()
			do.ProvideValue(di, slog.Default())
			config, err := NewConfig(di)
			if err != nil {
				t.Fatalf("NewConfig returned error: %v", err)
			}
			if config.GuestAbiManifestPath != tc.want {
				t.Fatalf("GuestAbiManifestPath = %q, want %q", config.GuestAbiManifestPath, tc.want)
			}
		})
	}
}
