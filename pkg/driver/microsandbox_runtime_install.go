package driver

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The v0.7 SDK resolves its home runtime as bin/msb plus the released
// libkrunfw filename (for example libkrunfw.so.5.6.1), and publishes these
// stable links beside it. A mounted runtime that provides only a link has no
// released filename for the resolver to find, so the installer rejects it
// instead of publishing a home the SDK cannot use.
const (
	microsandboxLibkrunfwSoname = "libkrunfw.so.5"
	microsandboxLibkrunfwLink   = "libkrunfw.so"
)

// installMicrosandboxRuntimePair copies the mounted runtime pair into the
// microsandbox home and returns the installed paths.
//
// The SDK resolves sandbox runtimes from its home, and agent-compose exports
// MicrosandboxHome as MSB_HOME. Installing the pair under the process user's
// ~/.microsandbox instead leaves that home without a resolvable runtime, and
// every sandbox start fails with "microsandbox runtime is not installed". The
// copy also keeps the deployment's runtime artifacts separate from the
// writable home that holds sandbox state.
func installMicrosandboxRuntimePair(homeDir string, msbPath string, libkrunfwPath string) (string, string, error) {
	libFilename := filepath.Base(libkrunfwPath)
	if libFilename == microsandboxLibkrunfwSoname || libFilename == microsandboxLibkrunfwLink {
		return "", "", fmt.Errorf("install microsandbox libkrunfw: %s has no released version in its filename; the SDK resolves its home runtime by the released libkrunfw.so.<version> file", libkrunfwPath)
	}

	binDir := filepath.Join(homeDir, "bin")
	libDir := filepath.Join(homeDir, "lib")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create microsandbox install bin dir: %w", err)
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create microsandbox install lib dir: %w", err)
	}
	installedMSBPath := filepath.Join(binDir, "msb")
	if err := copyFileWithMode(msbPath, installedMSBPath, 0o755); err != nil {
		return "", "", fmt.Errorf("install microsandbox msb: %w", err)
	}

	installedLibPath := filepath.Join(libDir, libFilename)
	if err := copyFileWithMode(libkrunfwPath, installedLibPath, 0o644); err != nil {
		return "", "", fmt.Errorf("install microsandbox libkrunfw: %w", err)
	}
	// Never replace the copied library with a link to itself. The released
	// filename is versioned, so the guards are defensive, but a same-name link
	// would leave home/lib with no loadable library at all.
	if libFilename != microsandboxLibkrunfwSoname {
		if err := symlinkForce(libFilename, filepath.Join(libDir, microsandboxLibkrunfwSoname)); err != nil {
			return "", "", fmt.Errorf("link microsandbox libkrunfw.so.5: %w", err)
		}
	}
	if libFilename != microsandboxLibkrunfwLink {
		if err := symlinkForce(microsandboxLibkrunfwSoname, filepath.Join(libDir, microsandboxLibkrunfwLink)); err != nil {
			return "", "", fmt.Errorf("link microsandbox libkrunfw.so: %w", err)
		}
	}
	return installedMSBPath, installedLibPath, nil
}

func copyFileWithMode(src string, dst string, mode os.FileMode) (err error) {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".agent-compose-microsandbox-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, source); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func symlinkForce(target string, path string) error {
	_ = os.Remove(path)
	return os.Symlink(target, path)
}
