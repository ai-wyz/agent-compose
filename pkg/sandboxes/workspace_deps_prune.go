package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/workspaces"
)

const (
	workspaceDepsNodeModules = "node_modules"
	workspaceDepsPnpmStore   = ".pnpm-store"
	workspaceDepsPackageJSON = "package.json"

	// workspaceDepsScanDepth bounds how far below the workspace root the pruner
	// lists directories. The root itself is depth zero.
	workspaceDepsScanDepth = 4
	// workspaceDepsPnpmStoreDepth is the deepest containing directory in which a
	// pnpm store is recognized: the workspace root or one of its children.
	workspaceDepsPnpmStoreDepth = 1
)

// WorkspaceDepsPruneBindings reports whether a scheduler still reuses a sandbox.
type WorkspaceDepsPruneBindings interface {
	SandboxHasSchedulerBinding(context.Context, string) (bool, error)
}

// WorkspaceDepsPruneResult describes what one prune attempt decided and did.
type WorkspaceDepsPruneResult struct {
	// SkipReason is empty when the sandbox was eligible.
	SkipReason string
	// Dirs lists the matched directories as slash-separated workspace-relative
	// paths. It is populated for dry runs as well.
	Dirs    []string
	Removed int
	DryRun  bool
}

// WorkspaceDepsPruner removes regenerable dependency directories from the
// workspace of a disposable sandbox when it stops. A disposable sandbox is one
// no scheduler will resume: it has no sticky binding, its stopped runtime is not
// retained, and its workspace is owned by the sandbox.
//
// The pruner is driven only by stop notifications held in memory. A sandbox it
// misses (daemon restart, unexpected runtime loss, a stop before the feature was
// enabled) keeps its dependencies until workspace cleanup reclaims the workspace.
type WorkspaceDepsPruner struct {
	Store    WorkspaceCleanupStore
	Locks    *LifecycleLocks
	Bindings WorkspaceDepsPruneBindings
	// GuestWorkspacePath locates declared volumes mounted inside the workspace.
	GuestWorkspacePath string
	Grace              time.Duration
	DryRun             bool
	ExtraDirs          []string
	Now                func() time.Time
	Logger             *slog.Logger

	mu      sync.Mutex
	pending []string
	timers  map[*time.Timer]struct{}
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// Notify queues a sandbox that just stopped. It never blocks, so it is safe to
// call while the caller holds the sandbox lifecycle lock.
func (p *WorkspaceDepsPruner) Notify(sandboxID string) {
	sandboxID = strings.TrimSpace(sandboxID)
	if p == nil || sandboxID == "" {
		return
	}
	if p.Grace <= 0 {
		p.enqueue(sandboxID)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if p.timers == nil {
		p.timers = make(map[*time.Timer]struct{})
	}
	var timer *time.Timer
	timer = time.AfterFunc(p.Grace, func() {
		p.mu.Lock()
		delete(p.timers, timer)
		p.mu.Unlock()
		p.enqueue(sandboxID)
	})
	p.timers[timer] = struct{}{}
}

func (p *WorkspaceDepsPruner) enqueue(sandboxID string) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if !slices.Contains(p.pending, sandboxID) {
		p.pending = append(p.pending, sandboxID)
	}
	wake := p.wake
	p.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Start runs the serial worker until ctx is cancelled or Shutdown is called.
func (p *WorkspaceDepsPruner) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.done != nil || p.stopped {
		p.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	p.wake = make(chan struct{}, 1)
	done, wake := p.done, p.wake
	queued := len(p.pending) > 0
	p.mu.Unlock()
	if queued {
		wake <- struct{}{}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-wake:
				p.drain(runCtx)
			}
		}
	}()
}

func (p *WorkspaceDepsPruner) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.stopped = true
	for timer := range p.timers {
		timer.Stop()
	}
	p.timers = nil
	p.pending = nil
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *WorkspaceDepsPruner) drain(ctx context.Context) {
	for ctx.Err() == nil {
		p.mu.Lock()
		if len(p.pending) == 0 {
			p.mu.Unlock()
			return
		}
		sandboxID := p.pending[0]
		p.pending = p.pending[1:]
		p.mu.Unlock()

		startedAt := time.Now()
		result, err := p.Prune(ctx, sandboxID)
		logger := p.logger().With("sandbox_id", sandboxID, "dry_run", result.DryRun, "duration", time.Since(startedAt).String())
		switch {
		case err != nil:
			logger.Warn("failed to prune workspace dependency directories", "removed", result.Removed, "matched", len(result.Dirs), "error", err)
		case result.SkipReason != "":
			logger.Debug("skipped workspace dependency prune", "reason", result.SkipReason)
		case len(result.Dirs) > 0 && result.DryRun:
			logger.Info("would prune workspace dependency directories", "matched", len(result.Dirs), "dirs", result.Dirs)
		case len(result.Dirs) > 0:
			logger.Info("pruned workspace dependency directories", "removed", result.Removed, "matched", len(result.Dirs), "dirs", result.Dirs)
		}
	}
}

// Prune applies the policy to one sandbox while holding its lifecycle lock.
func (p *WorkspaceDepsPruner) Prune(ctx context.Context, sandboxID string) (WorkspaceDepsPruneResult, error) {
	result := WorkspaceDepsPruneResult{DryRun: p.DryRun}
	if p.Store == nil {
		return result, fmt.Errorf("workspace dependency pruner store is not configured")
	}
	unlock := p.Locks.Lock(sandboxID)
	defer unlock()

	sandbox, err := p.Store.GetSandbox(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.SkipReason = "sandbox no longer exists"
			return result, nil
		}
		return result, err
	}
	stoppedAt, reason, err := p.eligibility(ctx, sandbox)
	if err != nil {
		return result, err
	}
	if reason != "" {
		result.SkipReason = reason
		return result, nil
	}
	workspacePath, err := (&WorkspaceCleaner{Store: p.Store}).safeWorkspacePath(sandbox)
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(workspacePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.SkipReason = "sandbox has no workspace directory"
			return result, nil
		}
		return result, fmt.Errorf("open sandbox workspace: %w", err)
	}
	defer func() { _ = root.Close() }()

	scan := workspaceDepsScan{root: root, extraDirs: p.ExtraDirs, protected: p.protectedWorkspacePaths(sandbox)}
	result.Dirs, err = scan.find(ctx)
	if err != nil {
		return result, fmt.Errorf("scan sandbox workspace: %w", err)
	}
	if len(result.Dirs) == 0 || p.DryRun {
		return result, nil
	}

	record := &domain.SandboxWorkspaceDepsPrune{
		State: domain.SandboxWorkspaceDepsPruneStatePruning, StoppedAt: stoppedAt, StartedAt: p.now(),
	}
	sandbox.WorkspaceDepsPrune = record
	if err := p.Store.UpdateSandbox(ctx, sandbox); err != nil {
		return result, fmt.Errorf("persist workspace dependency prune intent: %w", err)
	}
	for _, dir := range result.Dirs {
		// A cancelled prune leaves the record in the pruning state on purpose:
		// nothing retries it, and workspace cleanup removes what is left.
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := root.RemoveAll(filepath.FromSlash(dir)); err != nil {
			record.RemovedDirs = result.Removed
			record.LastError = err.Error()
			_ = p.Store.UpdateSandbox(context.WithoutCancel(ctx), sandbox)
			return result, fmt.Errorf("remove workspace dependency directory %q: %w", dir, err)
		}
		result.Removed++
	}
	record.State = domain.SandboxWorkspaceDepsPruneStatePruned
	record.CompletedAt = p.now()
	record.RemovedDirs = result.Removed
	record.LastError = ""
	if err := p.Store.UpdateSandbox(ctx, sandbox); err != nil {
		return result, fmt.Errorf("persist pruned workspace dependencies: %w", err)
	}
	_ = p.Store.AddEvent(ctx, sandbox.Summary.ID, domain.SandboxEvent{
		ID: uuid.NewString(), Type: "sandbox.workspace_deps_pruned", Level: "info",
		Message:   fmt.Sprintf("removed %d regenerable dependency directories from the stopped sandbox workspace; reinstall dependencies after resuming", result.Removed),
		CreatedAt: p.now(),
	})
	return result, nil
}

// eligibility returns the confirmed stop time of a disposable sandbox, or the
// reason it must be left alone. Every doubtful case is a reason to skip.
func (p *WorkspaceDepsPruner) eligibility(ctx context.Context, sandbox *domain.Sandbox) (time.Time, string, error) {
	stoppedAt, ok, err := (&WorkspaceCleaner{Store: p.Store}).workspaceEligibleAt(sandbox)
	if err != nil {
		return time.Time{}, "", err
	}
	if !ok {
		return time.Time{}, "sandbox has no confirmed current stop", nil
	}
	if p.now().Before(stoppedAt.Add(p.Grace)) {
		return time.Time{}, "grace period has not elapsed", nil
	}
	if sandbox.WorkspaceReclamation != nil {
		return time.Time{}, "workspace is reclaimed or being reclaimed", nil
	}
	if sandbox.Archive != nil {
		return time.Time{}, "sandbox is archived or being archived", nil
	}
	if record := sandbox.WorkspaceDepsPrune; record != nil && record.State == domain.SandboxWorkspaceDepsPruneStatePruned && record.StoppedAt.Equal(stoppedAt) {
		return time.Time{}, "dependencies were already pruned for this stop", nil
	}
	if EffectiveStoppedRuntimePolicy(sandbox) != domain.StoppedRuntimePolicyRemove {
		return time.Time{}, "stopped runtime is retained for resume", nil
	}
	if sandbox.Workspace != nil {
		mount, err := workspaces.DecodeFileWorkspaceMount(sandbox.Workspace.ConfigJSON)
		if err != nil {
			return time.Time{}, "workspace delivery cannot be determined", nil
		}
		if mount != nil {
			return time.Time{}, "workspace is an external mount", nil
		}
	}
	if p.Bindings == nil {
		return time.Time{}, "scheduler bindings cannot be checked", nil
	}
	bound, err := p.Bindings.SandboxHasSchedulerBinding(ctx, sandbox.Summary.ID)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("check scheduler binding: %w", err)
	}
	if bound {
		return time.Time{}, "sandbox is bound to a scheduler", nil
	}
	return stoppedAt, "", nil
}

// protectedWorkspacePaths returns the workspace-relative paths of declared
// volumes mounted inside the guest workspace.
func (p *WorkspaceDepsPruner) protectedWorkspacePaths(sandbox *domain.Sandbox) []string {
	guestRoot := path.Clean(filepath.ToSlash(strings.TrimSpace(p.GuestWorkspacePath)))
	if guestRoot == "." || guestRoot == "/" {
		return nil
	}
	var protected []string
	for _, mount := range sandbox.VolumeMounts {
		target := path.Clean(filepath.ToSlash(strings.TrimSpace(mount.Target)))
		relative, ok := strings.CutPrefix(target, guestRoot+"/")
		if ok && relative != "" {
			protected = append(protected, relative)
		}
	}
	return protected
}

func (p *WorkspaceDepsPruner) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *WorkspaceDepsPruner) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

type workspaceDepsScan struct {
	root      *os.Root
	extraDirs []string
	protected []string
}

func (s workspaceDepsScan) find(ctx context.Context) ([]string, error) {
	rootInfo, err := s.root.Stat(".")
	if err != nil {
		return nil, err
	}
	var matched []string
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := s.readDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			// DirEntry reports the entry itself, so a symlink to a directory is
			// neither matched nor followed.
			if !entry.IsDir() || entry.Name() == ".git" {
				continue
			}
			relative := path.Join(dir, entry.Name())
			if s.isProtected(relative) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			// A different device means another filesystem is mounted here.
			if !sameDevice(rootInfo, info) {
				continue
			}
			matches, descend := s.classify(dir, entry.Name(), depth)
			if matches {
				// Removing a directory that holds a volume mount point would
				// reach into the volume, so such a match is left in place.
				if !s.containsProtected(relative) {
					matched = append(matched, relative)
				}
				continue
			}
			if descend && depth < workspaceDepsScanDepth {
				if err := walk(relative, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(".", 0); err != nil {
		return nil, err
	}
	return matched, nil
}

func (s workspaceDepsScan) readDir(dir string) ([]os.DirEntry, error) {
	file, err := s.root.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	entries, err := file.ReadDir(-1)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

// classify decides whether the directory name inside dir is regenerable, and
// otherwise whether the scan should continue below it. A dependency directory
// that fails its guard is still never descended into.
func (s workspaceDepsScan) classify(dir, name string, depth int) (matches, descend bool) {
	switch name {
	case workspaceDepsNodeModules:
		info, err := s.root.Lstat(filepath.FromSlash(path.Join(dir, workspaceDepsPackageJSON)))
		return err == nil && info.Mode().IsRegular(), false
	case workspaceDepsPnpmStore:
		return depth <= workspaceDepsPnpmStoreDepth, false
	}
	if slices.Contains(s.extraDirs, name) {
		return true, false
	}
	return false, true
}

// isProtected reports whether relative is, or lies inside, a declared volume
// mount, so a volume is never matched or entered.
func (s workspaceDepsScan) isProtected(relative string) bool {
	for _, protected := range s.protected {
		if relative == protected || strings.HasPrefix(relative, protected+"/") {
			return true
		}
	}
	return false
}

// containsProtected reports whether a declared volume is mounted below relative.
func (s workspaceDepsScan) containsProtected(relative string) bool {
	for _, protected := range s.protected {
		if strings.HasPrefix(protected, relative+"/") {
			return true
		}
	}
	return false
}

// sameDevice reports whether two files live on the same filesystem. Missing
// platform metadata is treated as a different device so the caller skips it.
func sameDevice(a, b os.FileInfo) bool {
	left, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	right, ok := b.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return left.Dev == right.Dev
}
