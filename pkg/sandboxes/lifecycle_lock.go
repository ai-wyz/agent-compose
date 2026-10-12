package sandboxes

import (
	"strings"
	"sync"
)

// LifecycleLocks serializes state-changing operations for a sandbox within a
// daemon process. The lifecycle journal remains the durable crash-recovery
// boundary; these locks close the live remove/resume/start race.
type LifecycleLocks struct {
	mu    sync.Mutex
	locks map[string]*lifecycleLock

	stopObserver func(sandboxID string)
}

type lifecycleLock struct {
	mu    sync.Mutex
	users int
}

func NewLifecycleLocks() *LifecycleLocks {
	return &LifecycleLocks{locks: make(map[string]*lifecycleLock)}
}

func (l *LifecycleLocks) Lock(sandboxID string) func() {
	if l == nil {
		return func() {}
	}
	sandboxID = strings.TrimSpace(sandboxID)
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*lifecycleLock)
	}
	lock := l.locks[sandboxID]
	if lock == nil {
		lock = &lifecycleLock{}
		l.locks[sandboxID] = lock
	}
	lock.users++
	l.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		l.mu.Lock()
		lock.users--
		if lock.users == 0 && l.locks[sandboxID] == lock {
			delete(l.locks, sandboxID)
		}
		l.mu.Unlock()
	}
}

// SetStopObserver registers the callback told about every sandbox stop that
// completes through Lifecycle. LifecycleLocks is the one value every lifecycle
// user in the daemon already shares, so observing stops here covers each stop
// path without threading another dependency through them. The callback runs
// while the stopping caller still holds the sandbox lock and must not block.
func (l *LifecycleLocks) SetStopObserver(observer func(sandboxID string)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.stopObserver = observer
	l.mu.Unlock()
}

func (l *LifecycleLocks) notifyStopped(sandboxID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	observer := l.stopObserver
	l.mu.Unlock()
	if observer != nil {
		observer(strings.TrimSpace(sandboxID))
	}
}
