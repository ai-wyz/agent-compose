// Package guestabi owns the daemon/guest ABI contract: parsing, validating, and
// loading the guest ABI manifest a guest image publishes, and deciding whether
// this daemon release can speak it. It deliberately knows nothing about what a
// provider can do; the capability matrix derives that from a parsed manifest.
package guestabi

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Provider is one guest provider's published version and execution features.
// Feature names are opaque to this package: the daemon decides which ones it
// understands, so an unknown feature is preserved rather than rejected.
type Provider struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// Manifest is the guest ABI contract one guest image publishes.
type Manifest struct {
	GuestAbi  int                 `json:"guestAbi"`
	Providers map[string]Provider `json:"providers"`
}

// Parse decodes and validates a guest ABI manifest. A manifest that omits
// guestAbi is rejected rather than defaulted to a valid ABI, so an unversioned
// manifest can never be mistaken for a compatible one.
func Parse(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse guest abi manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate guest abi manifest: %w", err)
	}
	return manifest, nil
}

// LoadFile reads and parses a guest ABI manifest from path. It owns the file
// read, and it parses before returning, so a caller never receives a partially
// decoded manifest.
func LoadFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read guest abi manifest %q: %w", path, err)
	}
	manifest, err := Parse(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("load guest abi manifest %q: %w", path, err)
	}
	return manifest, nil
}

// Validate reports whether a manifest is structurally usable: it must declare a
// positive ABI, at least one provider, and a non-empty version for every
// provider. Feature names are intentionally not checked, because this release
// ignores features it does not understand.
func (m Manifest) Validate() error {
	if m.GuestAbi < 1 {
		return fmt.Errorf("guestAbi %d must be at least 1", m.GuestAbi)
	}
	if len(m.Providers) == 0 {
		return fmt.Errorf("manifest declares no providers")
	}
	// Sort the names so a manifest with several invalid providers always
	// reports the same one instead of following map iteration order.
	names := make([]string, 0, len(m.Providers))
	for name := range m.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(m.Providers[name].Version) == "" {
			return fmt.Errorf("provider %q declares an empty version", name)
		}
	}
	return nil
}
