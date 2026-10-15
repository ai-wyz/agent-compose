package config

import (
	"os"
	"strings"
)

// guestABIManifestEnv names the optional path to a guest ABI manifest. The
// default is empty: the daemon's built-in static provider matrix is
// authoritative unless an operator points at a manifest a guest image
// published.
const guestABIManifestEnv = "GUEST_ABI_MANIFEST"

// loadGuestABIManifestPath resolves the optional guest ABI manifest path. A
// blank value stays empty rather than defaulting to a file, so opting in to the
// guest-provided matrix is always explicit.
func loadGuestABIManifestPath() string {
	return strings.TrimSpace(os.Getenv(guestABIManifestEnv))
}
