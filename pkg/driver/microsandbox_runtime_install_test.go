package driver

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallMicrosandboxRuntimePairPublishesVersionedLibrary(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	sourceMSB := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "bin", "msb"), 0o755)
	sourceLib := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "lib", "libkrunfw.so.5.6.1"), 0o644)

	installedMSB, installedLib, err := installMicrosandboxRuntimePair(home, sourceMSB, sourceLib)
	if err != nil {
		t.Fatalf("installMicrosandboxRuntimePair: %v", err)
	}

	wantMSB := filepath.Join(home, "bin", "msb")
	wantLib := filepath.Join(home, "lib", "libkrunfw.so.5.6.1")
	if installedMSB != wantMSB {
		t.Fatalf("installed msb = %q, want %q", installedMSB, wantMSB)
	}
	if installedLib != wantLib {
		t.Fatalf("installed libkrunfw = %q, want %q", installedLib, wantLib)
	}
	assertMicrosandboxInstallFile(t, wantMSB, sourceMSB, 0o755)
	assertMicrosandboxInstallFile(t, wantLib, sourceLib, 0o644)
	for link, target := range map[string]string{
		microsandboxLibkrunfwSoname: "libkrunfw.so.5.6.1",
		microsandboxLibkrunfwLink:   microsandboxLibkrunfwSoname,
	} {
		path := filepath.Join(home, "lib", link)
		got, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("readlink %s: %v", path, err)
		}
		if got != target {
			t.Fatalf("%s -> %s, want %s", path, got, target)
		}
	}
}

func TestInstallMicrosandboxRuntimePairRejectsBareSoname(t *testing.T) {
	for _, name := range []string{microsandboxLibkrunfwSoname, microsandboxLibkrunfwLink} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			sourceDir := t.TempDir()
			sourceMSB := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "bin", "msb"), 0o755)
			sourceLib := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "lib", name), 0o644)

			if _, _, err := installMicrosandboxRuntimePair(home, sourceMSB, sourceLib); err == nil {
				t.Fatalf("installMicrosandboxRuntimePair accepted a bare %s", name)
			} else if !strings.Contains(err.Error(), "released libkrunfw.so.<version>") {
				t.Fatalf("installMicrosandboxRuntimePair error = %v, want an actionable released-filename error", err)
			}
			if _, err := os.Stat(filepath.Join(home, "bin", "msb")); !os.IsNotExist(err) {
				t.Fatalf("rejected install wrote home/bin/msb: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(home, "lib", name)); !os.IsNotExist(err) {
				t.Fatalf("rejected install wrote a library or self-link at %s: %v", name, err)
			}
		})
	}
}

// A home left by the previous installer can contain a same-name link that
// points at itself. Reinstalling must replace it with a real library and the
// normal soname links instead of preserving an unloadable path.
func TestInstallMicrosandboxRuntimePairRepairsSelfReferentialLink(t *testing.T) {
	home := t.TempDir()
	sourceDir := t.TempDir()
	sourceMSB := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "bin", "msb"), 0o755)
	sourceLib := writeMicrosandboxInstallFixture(t, filepath.Join(sourceDir, "lib", "libkrunfw.so.5.6.1"), 0o644)
	libDir := filepath.Join(home, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("mkdir lib dir: %v", err)
	}
	if err := os.Symlink(microsandboxLibkrunfwSoname, filepath.Join(libDir, microsandboxLibkrunfwSoname)); err != nil {
		t.Fatalf("seed self-referential link: %v", err)
	}

	_, installedLib, err := installMicrosandboxRuntimePair(home, sourceMSB, sourceLib)
	if err != nil {
		t.Fatalf("installMicrosandboxRuntimePair: %v", err)
	}
	assertMicrosandboxInstallFile(t, installedLib, sourceLib, 0o644)
	if got, err := os.Readlink(filepath.Join(libDir, microsandboxLibkrunfwSoname)); err != nil {
		t.Fatalf("readlink %s: %v", microsandboxLibkrunfwSoname, err)
	} else if got != "libkrunfw.so.5.6.1" {
		t.Fatalf("%s -> %s, want libkrunfw.so.5.6.1", microsandboxLibkrunfwSoname, got)
	}
}

func writeMicrosandboxInstallFixture(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	content := []byte("fixture:" + filepath.Base(path))
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func assertMicrosandboxInstallFile(t *testing.T, gotPath string, wantPath string, wantMode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read %s: %v", gotPath, err)
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s does not match %s", gotPath, wantPath)
	}
	info, err := os.Stat(gotPath)
	if err != nil {
		t.Fatalf("stat %s: %v", gotPath, err)
	}
	if info.Mode().Perm() != wantMode {
		t.Fatalf("%s mode = %v, want %v", gotPath, info.Mode().Perm(), wantMode)
	}
}
