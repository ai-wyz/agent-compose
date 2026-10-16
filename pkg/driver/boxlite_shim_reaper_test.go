//go:build linux

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// startStandInProcess starts a long-lived process and guarantees the test does
// not leave it behind: the reaper kills it in the positive test, and cleanup
// kills whatever the test asserts must stay alive.
func startStandInProcess(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(path, "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// startBoxliteShimStandIn starts a long-lived process whose command line names
// it as a boxlite shim, which is what the reaper identifies before it signals
// anything. /bin/sleep under a boxlite-named symlink gives that shape without
// the driver's cgo runtime.
func startBoxliteShimStandIn(t *testing.T) *exec.Cmd {
	t.Helper()
	shimPath := filepath.Join(t.TempDir(), "boxlite-shim")
	if err := os.Symlink("/bin/sleep", shimPath); err != nil {
		t.Skipf("cannot create a boxlite shim stand-in: %v", err)
	}
	return startStandInProcess(t, shimPath)
}

func writeBoxliteShimPID(t *testing.T, runtimeHome, boxID string, pid int) {
	t.Helper()
	boxDir := filepath.Join(runtimeHome, "boxes", boxID)
	if err := os.MkdirAll(boxDir, 0o755); err != nil {
		t.Fatalf("create box directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boxDir, boxliteShimPIDFileName), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatalf("write shim pid file: %v", err)
	}
}

// TestReapBoxliteShimProcessesReleasesTheShimOfAFailedStart pins the fix for the
// leak the test round measured: a failed boxlite start leaves its detached shim
// running with the VM's /dev/kvm handle open, and repeated failures accumulate
// handles until the host's kvm module cannot be unloaded.
func TestReapBoxliteShimProcessesReleasesTheShimOfAFailedStart(t *testing.T) {
	runtimeHome := t.TempDir()
	cmd := startBoxliteShimStandIn(t)
	writeBoxliteShimPID(t, runtimeHome, "box-of-failed-start", cmd.Process.Pid)

	reaped, err := ReapBoxliteShimProcesses(runtimeHome, "box-of-failed-start")
	if err != nil {
		t.Fatalf("ReapBoxliteShimProcesses returned error: %v", err)
	}
	if !reaped {
		t.Fatal("ReapBoxliteShimProcesses reported nothing to reap for a failed start's shim")
	}
	if boxliteProcessAlive(cmd.Process.Pid) {
		t.Fatalf("boxlite shim process %d is still alive after reaping", cmd.Process.Pid)
	}
}

// TestReapBoxliteShimProcessesRefusesAProcessItDoesNotOwn pins the safety rule
// the reaper needs because pids are reused: a stale pid file must not turn into
// a signal to an unrelated process.
func TestReapBoxliteShimProcessesRefusesAProcessItDoesNotOwn(t *testing.T) {
	runtimeHome := t.TempDir()
	cmd := startStandInProcess(t, "/bin/sleep")
	writeBoxliteShimPID(t, runtimeHome, "box-with-stale-pid", cmd.Process.Pid)

	reaped, err := ReapBoxliteShimProcesses(runtimeHome, "box-with-stale-pid")
	if err == nil {
		t.Fatal("ReapBoxliteShimProcesses accepted a pid that is not a boxlite process")
	}
	if reaped {
		t.Fatal("ReapBoxliteShimProcesses reported reaping a process it must not signal")
	}
	if !boxliteProcessAlive(cmd.Process.Pid) {
		t.Fatalf("unrelated process %d was signalled", cmd.Process.Pid)
	}
}

func TestReapBoxliteShimProcessesSkipsABoxWithoutAShimPID(t *testing.T) {
	runtimeHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runtimeHome, "boxes", "box-never-started"), 0o755); err != nil {
		t.Fatalf("create box directory: %v", err)
	}

	reaped, err := reapBoxliteShimProcesses(runtimeHome, "box-never-started", boxliteReapTiming{pidAttempts: 1, exitAttempts: 1})
	if err != nil {
		t.Fatalf("reapBoxliteShimProcesses returned error: %v", err)
	}
	if reaped {
		t.Fatal("reapBoxliteShimProcesses reported reaping a shim that was never recorded")
	}
}

func TestReapBoxliteShimProcessesRejectsMalformedInput(t *testing.T) {
	runtimeHome := t.TempDir()
	boxDir := filepath.Join(runtimeHome, "boxes", "box-with-bad-pid")
	if err := os.MkdirAll(boxDir, 0o755); err != nil {
		t.Fatalf("create box directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boxDir, boxliteShimPIDFileName), []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("write malformed shim pid file: %v", err)
	}

	if _, err := reapBoxliteShimProcesses(runtimeHome, "box-with-bad-pid", boxliteReapTiming{pidAttempts: 1, exitAttempts: 1}); err == nil {
		t.Fatal("reapBoxliteShimProcesses accepted a malformed shim pid file")
	}
	if _, err := ReapBoxliteShimProcesses(runtimeHome, "../outside-the-home"); err == nil {
		t.Fatal("ReapBoxliteShimProcesses accepted a box id that is not a single path element")
	}
	if reaped, err := ReapBoxliteShimProcesses("", "box-with-bad-pid"); err != nil || reaped {
		t.Fatalf("ReapBoxliteShimProcesses without a runtime home = (%v, %v), want (false, nil)", reaped, err)
	}
}

func TestWaitForBoxliteShimPIDWaitsForALatePIDFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), boxliteShimPIDFileName)
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(pidPath, []byte("1234\n"), 0o600)
	}()

	pid, found, err := waitForBoxliteShimPID(pidPath, 40, 5*time.Millisecond)
	<-done
	if err != nil {
		t.Fatalf("waitForBoxliteShimPID returned error: %v", err)
	}
	if !found || pid != 1234 {
		t.Fatalf("waitForBoxliteShimPID = (%d, %v), want (1234, true)", pid, found)
	}
}
