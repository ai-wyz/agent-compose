package app

import (
	"fmt"
	"log/slog"

	"github.com/chaitin/agent-compose/pkg/capmatrix"
	"github.com/chaitin/agent-compose/pkg/guestabi"
)

// engineProviderCapabilities resolves the provider facts for the engine
// capability snapshot. A configured manifest path opts in to the guest ABI
// contract; an empty path keeps the static declaration byte-for-byte. Every
// manifest failure warns and falls back to the static declaration, so a missing
// or stale manifest cannot take the daemon down.
func engineProviderCapabilities(manifestPath string, logger *slog.Logger) ([]capmatrix.ProviderCapabilities, error) {
	if manifestPath == "" {
		return capmatrix.DeclaredProviders()
	}
	providers, err := providersFromGuestABIManifest(manifestPath)
	if err != nil {
		logger.Warn("ignoring guest abi manifest and using the static provider matrix",
			"path", manifestPath,
			"error", err,
		)
		return capmatrix.DeclaredProviders()
	}
	return providers, nil
}

// providersFromGuestABIManifest loads, validates, and maps a manifest into
// provider facts. It touches nothing but the manifest file, so the snapshot's
// no-runtime-probe invariant still holds.
func providersFromGuestABIManifest(path string) ([]capmatrix.ProviderCapabilities, error) {
	manifest, err := guestabi.LoadFile(path)
	if err != nil {
		return nil, err
	}
	if err := manifest.VerifyCompatible(); err != nil {
		return nil, err
	}
	providers, err := capmatrix.DeclaredProvidersFromManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("map guest abi manifest providers: %w", err)
	}
	return providers, nil
}
