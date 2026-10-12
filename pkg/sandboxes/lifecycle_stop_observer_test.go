package sandboxes

import (
	"context"
	"errors"
	"testing"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestLifecycleStopNotifiesObserverOnlyForStopsItPerformed(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	locks := NewLifecycleLocks()
	var notified []string
	locks.SetStopObserver(func(sandboxID string) { notified = append(notified, sandboxID) })

	running := lifecycleTestSession("session-running", driverpkg.RuntimeDriverDocker, domain.VMStatusRunning)
	lifecycle := Lifecycle{Store: &fakeLifecycleStore{session: running}, Driver: &fakeSandboxDriver{}, Locks: locks}
	if _, err := lifecycle.StopLoaded(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	if len(notified) != 1 || notified[0] != running.Summary.ID {
		t.Fatalf("notified = %v, want the stopped sandbox", notified)
	}

	// A sandbox whose stop is already confirmed is not stopped again, so there is
	// nothing new to report.
	stopped := lifecycleTestSession("session-already-stopped", driverpkg.RuntimeDriverDocker, domain.VMStatusStopped)
	lifecycle.Store = &fakeLifecycleStore{session: stopped, vmState: domain.VMState{
		StartAttemptedAt: now.Add(-2 * time.Hour), StoppedAt: now.Add(-time.Hour),
	}}
	if outcome, err := lifecycle.StopLoaded(context.Background(), stopped); err != nil || outcome.Changed() {
		t.Fatalf("StopLoaded already stopped = %#v/%v", outcome, err)
	}
	if len(notified) != 1 {
		t.Fatalf("notified = %v, want no notification for a repeated stop", notified)
	}
}

func TestLifecycleStopDoesNotNotifyObserverWhenStopFails(t *testing.T) {
	locks := NewLifecycleLocks()
	notified := 0
	locks.SetStopObserver(func(string) { notified++ })
	releaseErr := errors.New("runtime release failed")
	session := lifecycleTestSession("session-release-failure", driverpkg.RuntimeDriverDocker, domain.VMStatusRunning)
	session.StoppedRuntimePolicy = domain.StoppedRuntimePolicyRemove
	lifecycle := Lifecycle{
		Config: &appconfig.Config{SandboxRoot: t.TempDir()},
		Store:  &fakeLifecycleStore{session: session},
		Driver: &recordingSandboxDriver{releaseErr: releaseErr},
		Locks:  locks,
	}

	if _, err := lifecycle.StopLoaded(context.Background(), session); !errors.Is(err, releaseErr) {
		t.Fatalf("StopLoaded error = %v, want %v", err, releaseErr)
	}
	if notified != 0 {
		t.Fatalf("observer notified %d times for a failed stop", notified)
	}
}

func TestLifecycleLocksStopObserverIsOptional(t *testing.T) {
	var nilLocks *LifecycleLocks
	nilLocks.SetStopObserver(func(string) {})
	nilLocks.notifyStopped("sandbox")
	NewLifecycleLocks().notifyStopped("sandbox")
}
