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

// startBoxliteMentioningStandIn starts a long-lived process whose *arguments*
// name boxlite while the program it runs does not. That is the look-alike the
// reaper must refuse: a pid recorded on disk can be reused by any process, and
// only the program it runs may decide whether it is the shim.
func startBoxliteMentioningStandIn(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	cmd.Args[0] = filepath.Join(t.TempDir(), "boxlite-notes")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start /bin/sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
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

// TestReapBoxliteShimProcessesRefusesAProcessThatOnlyMentionsBoxlite pins the
// tightened identification: matching the whole command line accepted any
// process with "boxlite" anywhere in its arguments, which a reused pid only has
// to mention once to be killed.
func TestReapBoxliteShimProcessesRefusesAProcessThatOnlyMentionsBoxlite(t *testing.T) {
	runtimeHome := t.TempDir()
	cmd := startBoxliteMentioningStandIn(t)
	writeBoxliteShimPID(t, runtimeHome, "box-with-a-lookalike-pid", cmd.Process.Pid)

	reaped, err := ReapBoxliteShimProcesses(runtimeHome, "box-with-a-lookalike-pid")
	if err == nil {
		t.Fatal("ReapBoxliteShimProcesses accepted a pid whose arguments merely mention boxlite")
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

// TestWaitForBoxliteShimPIDWaitsForAnIncompletePIDFile pins the other half of
// that window: boxlite creates the pid file before it writes the value, so a
// read that lands in between sees empty content. Reporting that as a malformed
// file leaves the shim and its /dev/kvm handle behind, which is the leak the
// retry exists to close.
func TestWaitForBoxliteShimPIDWaitsForAnIncompletePIDFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), boxliteShimPIDFileName)
	if err := os.WriteFile(pidPath, nil, 0o600); err != nil {
		t.Fatalf("create the empty shim pid file: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(pidPath, []byte("4321\n"), 0o600)
	}()

	pid, found, err := waitForBoxliteShimPID(pidPath, 40, 5*time.Millisecond)
	<-done
	if err != nil {
		t.Fatalf("waitForBoxliteShimPID returned error: %v", err)
	}
	if !found || pid != 4321 {
		t.Fatalf("waitForBoxliteShimPID = (%d, %v), want (4321, true)", pid, found)
	}
}

// TestWaitForBoxliteShimPIDReportsAFileThatNeverHoldsAPID keeps the widened
// retry from turning malformed input into a silent "nothing to reap": a file
// that stays invalid is still an error, while one that never appears is not.
func TestWaitForBoxliteShimPIDReportsAFileThatNeverHoldsAPID(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), boxliteShimPIDFileName)
	if err := os.WriteFile(pidPath, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("write malformed shim pid file: %v", err)
	}

	pid, found, err := waitForBoxliteShimPID(pidPath, 3, time.Millisecond)
	if err == nil {
		t.Fatal("waitForBoxliteShimPID accepted a pid file that never holds a pid")
	}
	if found || pid != 0 {
		t.Fatalf("waitForBoxliteShimPID = (%d, %v), want (0, false)", pid, found)
	}

	if pid, found, err := waitForBoxliteShimPID(filepath.Join(t.TempDir(), boxliteShimPIDFileName), 2, time.Millisecond); err != nil || found || pid != 0 {
		t.Fatalf("waitForBoxliteShimPID for a missing file = (%d, %v, %v), want (0, false, nil)", pid, found, err)
	}
}
