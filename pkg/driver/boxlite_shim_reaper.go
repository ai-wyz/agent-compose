package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// boxliteShimPIDFileName is where boxlite records the pid of the detached VM
	// shim it started for a box.
	boxliteShimPIDFileName = "shim.pid"
	// boxliteShimPIDAttempts/boxliteShimPIDInterval bound the wait for that file
	// after a failed start: a start that failed after boxlite handed control to
	// the shim has written it already, while one cancelled earlier may write it
	// just after cleanup looks.
	boxliteShimPIDAttempts = 12
	boxliteShimPIDInterval = 250 * time.Millisecond
	// boxliteShimExitAttempts/boxliteShimExitInterval bound the wait for a
	// signalled shim to leave, so a signal that was ignored is reported instead
	// of assumed to have worked.
	boxliteShimExitAttempts = 20
	boxliteShimExitInterval = 100 * time.Millisecond
)

// ReapBoxliteShimProcesses terminates the detached VM shim a failed boxlite
// start left behind, and reports whether it terminated one.
//
// boxlite reports a failed start without releasing the shim or the bwrap jail
// around it, and both keep the VM's /dev/kvm handle open: repeated failures
// accumulate handles until the host's kvm module can no longer be unloaded
// (test round D2b). Releasing the process is therefore the caller's job.
//
// The signal is only sent to a process whose command line identifies it as part
// of a boxlite runtime, because a pid recorded for a box that has since been
// removed must never become a signal to an unrelated process that reused the
// id. Callers reap immediately after a failed start, while the pid is current.
//
// It is exported because its only caller is the cgo boxlite start path, which
// is compiled out of builds that do not include that driver.
func ReapBoxliteShimProcesses(runtimeHome, boxID string) (bool, error) {
	return reapBoxliteShimProcesses(runtimeHome, boxID, defaultBoxliteReapTiming())
}

// boxliteReapTiming bounds both waits the reaper performs. It is a value rather
// than four more parameters because only the retry loops care about it, and
// tests shorten it so they do not spend seconds waiting for a pid file or a
// process that never appears.
type boxliteReapTiming struct {
	pidAttempts  int
	pidInterval  time.Duration
	exitAttempts int
	exitInterval time.Duration
}

func defaultBoxliteReapTiming() boxliteReapTiming {
	return boxliteReapTiming{
		pidAttempts:  boxliteShimPIDAttempts,
		pidInterval:  boxliteShimPIDInterval,
		exitAttempts: boxliteShimExitAttempts,
		exitInterval: boxliteShimExitInterval,
	}
}

func reapBoxliteShimProcesses(runtimeHome, boxID string, timing boxliteReapTiming) (bool, error) {
	pidPath, err := boxliteShimPIDPath(runtimeHome, boxID)
	if err != nil || pidPath == "" {
		return false, err
	}
	pid, found, err := waitForBoxliteShimPID(pidPath, timing.pidAttempts, timing.pidInterval)
	if err != nil || !found {
		return false, err
	}
	isShim, err := identifyBoxliteShimProcess(pid)
	if err != nil {
		return false, err
	}
	if !isShim {
		return false, fmt.Errorf("boxlite box %s records pid %d, which does not belong to a boxlite process", strings.TrimSpace(boxID), pid)
	}
	if err := terminateBoxliteShim(pid); err != nil {
		return false, err
	}
	if !waitForBoxliteShimExit(pid, timing.exitAttempts, timing.exitInterval) {
		return false, fmt.Errorf("boxlite shim process %d is still running after SIGKILL", pid)
	}
	return true, nil
}

// boxliteShimPIDPath returns the pid file boxlite writes for a box. It refuses
// anything that is not a single path element, so a malformed box id can never
// point the lookup outside the runtime home.
func boxliteShimPIDPath(runtimeHome, boxID string) (string, error) {
	runtimeHome = strings.TrimSpace(runtimeHome)
	boxID = strings.TrimSpace(boxID)
	if runtimeHome == "" || boxID == "" {
		return "", nil
	}
	if boxID == "." || boxID == ".." || strings.ContainsAny(boxID, `/\`) {
		return "", fmt.Errorf("boxlite box id %q is not a single path element", boxID)
	}
	return filepath.Join(runtimeHome, "boxes", boxID, boxliteShimPIDFileName), nil
}

func waitForBoxliteShimPID(pidPath string, attempts int, interval time.Duration) (int, bool, error) {
	for attempt := 0; attempt < attempts; attempt++ {
		pid, err := readBoxliteShimPID(pidPath)
		switch {
		case err == nil:
			return pid, true, nil
		case errors.Is(err, fs.ErrNotExist):
			if attempt < attempts-1 {
				time.Sleep(interval)
			}
		default:
			return 0, false, err
		}
	}
	return 0, false, nil
}

func readBoxliteShimPID(pidPath string) (int, error) {
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse boxlite shim pid %q: %w", value, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("boxlite shim pid %d is not a valid process id", pid)
	}
	return pid, nil
}

func waitForBoxliteShimExit(pid int, attempts int, interval time.Duration) bool {
	for attempt := 0; attempt < attempts; attempt++ {
		if !boxliteProcessAlive(pid) {
			return true
		}
		if attempt < attempts-1 {
			time.Sleep(interval)
		}
	}
	return !boxliteProcessAlive(pid)
}
