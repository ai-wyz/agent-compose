package sandboxes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/sandboxes"
	"github.com/chaitin/agent-compose/pkg/storage/sandboxstore"
)

type workspaceDepsTestBindings struct {
	bound bool
	err   error
}

func (b workspaceDepsTestBindings) SandboxHasSchedulerBinding(context.Context, string) (bool, error) {
	return b.bound, b.err
}

var workspaceDepsTestNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// newWorkspaceDepsSandbox returns a stopped disposable sandbox and a pruner for it.
func newWorkspaceDepsSandbox(t *testing.T) (*sandboxstore.Store, *domain.Sandbox, *sandboxes.WorkspaceDepsPruner) {
	t.Helper()
	store, sandbox := newWorkspaceCleanupSandbox(t, workspaceDepsTestNow.Add(-time.Minute))
	sandbox.StoppedRuntimePolicy = domain.StoppedRuntimePolicyRemove
	if err := store.UpdateSandbox(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	pruner := &sandboxes.WorkspaceDepsPruner{
		Store: store, Locks: sandboxes.NewLifecycleLocks(), Bindings: workspaceDepsTestBindings{},
		GuestWorkspacePath: "/workspace", Now: func() time.Time { return workspaceDepsTestNow },
	}
	return store, sandbox, pruner
}

func writeWorkspaceDepsFile(t *testing.T, sandbox *domain.Sandbox, relative string) string {
	t.Helper()
	target := filepath.Join(sandbox.Summary.WorkspacePath, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(relative), 0o644); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertWorkspaceDepsPaths(t *testing.T, sandbox *domain.Sandbox, kept, removed []string) {
	t.Helper()
	for _, relative := range kept {
		if _, err := os.Lstat(filepath.Join(sandbox.Summary.WorkspacePath, filepath.FromSlash(relative))); err != nil {
			t.Errorf("%s should be kept: %v", relative, err)
		}
	}
	for _, relative := range removed {
		if _, err := os.Lstat(filepath.Join(sandbox.Summary.WorkspacePath, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Errorf("%s should be removed, lstat error = %v", relative, err)
		}
	}
}

func TestWorkspaceDepsPrunerRemovesDependenciesAndKeepsSources(t *testing.T) {
	ctx := context.Background()
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	for _, relative := range []string{
		"README.md",
		"ui/package.json",
		"ui/src/index.ts",
		"ui/node_modules/react/index.js",
		"ui/node_modules/.pnpm/react/index.js",
		".pnpm-store/v3/files/00/abc",
		"services/api/package.json",
		"services/api/node_modules/express/index.js",
		"services/.pnpm-store/v3/files/00/def",
	} {
		writeWorkspaceDepsFile(t, sandbox, relative)
	}
	// pnpm hard-links node_modules files into the store.
	linked := filepath.Join(sandbox.Summary.WorkspacePath, "ui", "node_modules", "linked.js")
	if err := os.Link(filepath.Join(sandbox.Summary.WorkspacePath, ".pnpm-store", "v3", "files", "00", "abc"), linked); err != nil {
		t.Fatal(err)
	}

	result, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".pnpm-store", "services/.pnpm-store", "services/api/node_modules", "ui/node_modules"}
	if result.SkipReason != "" || result.Removed != len(want) || !slices.Equal(result.Dirs, want) {
		t.Fatalf("result = %#v, want dirs %v", result, want)
	}
	assertWorkspaceDepsPaths(t, sandbox,
		[]string{"README.md", "ui/package.json", "ui/src/index.ts", "services/api/package.json"}, want)

	pruned, err := store.GetSandbox(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	record := pruned.WorkspaceDepsPrune
	if record == nil || record.State != domain.SandboxWorkspaceDepsPruneStatePruned || record.RemovedDirs != len(want) ||
		!record.StoppedAt.Equal(workspaceDepsTestNow.Add(-time.Minute)) || record.CompletedAt.IsZero() || record.LastError != "" {
		t.Fatalf("prune record = %#v", record)
	}
	events, err := store.ListEvents(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(event domain.SandboxEvent) bool { return event.Type == "sandbox.workspace_deps_pruned" }) {
		t.Fatalf("events = %#v, want sandbox.workspace_deps_pruned", events)
	}
}

func TestWorkspaceDepsPrunerLeavesGuardedDirectories(t *testing.T) {
	ctx := context.Background()
	_, sandbox, pruner := newWorkspaceDepsSandbox(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "kept.js"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		// No package.json beside it.
		"vendor/node_modules/pkg/index.js",
		// Nested in a guarded node_modules that is never entered.
		"vendor/node_modules/pkg/package.json",
		"vendor/node_modules/pkg/node_modules/dep/index.js",
		// A pnpm store deeper than a child of the workspace root.
		"a/b/.pnpm-store/v3/file",
		// Repository metadata is never entered.
		".git/package.json",
		".git/node_modules/hook/index.js",
		// Below the scan depth.
		"d1/d2/d3/d4/d5/package.json",
		"d1/d2/d3/d4/d5/node_modules/pkg/index.js",
		// Not on the default list.
		"py/.venv/bin/python",
		"linked/package.json",
	} {
		writeWorkspaceDepsFile(t, sandbox, relative)
	}
	if err := os.Symlink(outside, filepath.Join(sandbox.Summary.WorkspacePath, "linked", "node_modules")); err != nil {
		t.Fatal(err)
	}

	result, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkipReason != "" || len(result.Dirs) != 0 || result.Removed != 0 {
		t.Fatalf("result = %#v, want nothing matched", result)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{
		"vendor/node_modules/pkg/node_modules/dep/index.js", "a/b/.pnpm-store/v3/file", ".git/node_modules/hook/index.js",
		"d1/d2/d3/d4/d5/node_modules/pkg/index.js", "py/.venv/bin/python", "linked/node_modules",
	}, nil)
	if _, err := os.Stat(filepath.Join(outside, "kept.js")); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
}

func TestWorkspaceDepsPrunerMatchesAtScanDepthBoundary(t *testing.T) {
	_, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "d1/d2/d3/d4/package.json")
	writeWorkspaceDepsFile(t, sandbox, "d1/d2/d3/d4/node_modules/pkg/index.js")

	result, err := pruner.Prune(context.Background(), sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Dirs, []string{"d1/d2/d3/d4/node_modules"}) || result.Removed != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestWorkspaceDepsPrunerExtraDirs(t *testing.T) {
	_, sandbox, pruner := newWorkspaceDepsSandbox(t)
	pruner.ExtraDirs = []string{".venv"}
	writeWorkspaceDepsFile(t, sandbox, "py/.venv/bin/python")
	writeWorkspaceDepsFile(t, sandbox, "py/target/out.bin")

	result, err := pruner.Prune(context.Background(), sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Dirs, []string{"py/.venv"}) {
		t.Fatalf("result = %#v", result)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"py/target/out.bin"}, []string{"py/.venv"})
}

func TestWorkspaceDepsPrunerProtectsDeclaredVolumes(t *testing.T) {
	ctx := context.Background()
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	sandbox.VolumeMounts = []domain.SandboxVolumeMount{
		{Type: "bind", Target: "/workspace/cache/node_modules"},
		{Type: "bind", Target: "/workspace/app/node_modules/.cache"},
		{Type: "bind", Target: "/data/elsewhere"},
	}
	if err := store.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"cache/package.json", "cache/node_modules/pkg/index.js",
		"app/package.json", "app/node_modules/pkg/index.js",
		"web/package.json", "web/node_modules/pkg/index.js",
	} {
		writeWorkspaceDepsFile(t, sandbox, relative)
	}

	result, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Dirs, []string{"web/node_modules"}) {
		t.Fatalf("result = %#v, want only the unprotected directory", result)
	}
	assertWorkspaceDepsPaths(t, sandbox,
		[]string{"cache/node_modules/pkg/index.js", "app/node_modules/pkg/index.js"}, []string{"web/node_modules"})
}

func TestWorkspaceDepsPrunerSkipsIneligibleSandboxes(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *sandboxstore.Store, *domain.Sandbox, *sandboxes.WorkspaceDepsPruner)
		reason  string
	}{
		{name: "running", reason: "sandbox has no confirmed current stop", prepare: func(t *testing.T, store *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.Summary.VMStatus = domain.VMStatusRunning
		}},
		{name: "restarted after stop", reason: "sandbox has no confirmed current stop", prepare: func(t *testing.T, store *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			vmState, err := store.GetVMState(sandbox.Summary.ID)
			if err != nil {
				t.Fatal(err)
			}
			vmState.StartAttemptedAt = vmState.StoppedAt.Add(time.Second)
			if err := store.SaveVMState(sandbox.Summary.ID, vmState); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "grace period", reason: "grace period has not elapsed", prepare: func(_ *testing.T, _ *sandboxstore.Store, _ *domain.Sandbox, pruner *sandboxes.WorkspaceDepsPruner) {
			pruner.Grace = time.Hour
		}},
		{name: "retained runtime", reason: "stopped runtime is retained for resume", prepare: func(_ *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.StoppedRuntimePolicy = domain.StoppedRuntimePolicyRetain
		}},
		{name: "legacy sandbox without policy", reason: "stopped runtime is retained for resume", prepare: func(_ *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.StoppedRuntimePolicy = ""
		}},
		{name: "mounted workspace", reason: "workspace is an external mount", prepare: func(t *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			source := t.TempDir()
			sandbox.Workspace = &domain.SandboxWorkspace{ID: "workspace", Type: "file",
				ConfigJSON: `{"mode":"mount","source_path":"` + source + `","project_root":"` + source + `","target":""}`}
		}},
		{name: "unreadable workspace delivery", reason: "workspace delivery cannot be determined", prepare: func(_ *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.Workspace = &domain.SandboxWorkspace{ID: "workspace", Type: "file", ConfigJSON: `{"mode":`}
		}},
		{name: "scheduler binding", reason: "sandbox is bound to a scheduler", prepare: func(_ *testing.T, _ *sandboxstore.Store, _ *domain.Sandbox, pruner *sandboxes.WorkspaceDepsPruner) {
			pruner.Bindings = workspaceDepsTestBindings{bound: true}
		}},
		{name: "no binding source", reason: "scheduler bindings cannot be checked", prepare: func(_ *testing.T, _ *sandboxstore.Store, _ *domain.Sandbox, pruner *sandboxes.WorkspaceDepsPruner) {
			pruner.Bindings = nil
		}},
		{name: "workspace reclamation started", reason: "workspace is reclaimed or being reclaimed", prepare: func(_ *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.WorkspaceReclamation = &domain.SandboxWorkspaceReclamation{State: domain.SandboxWorkspaceReclamationStateReclaiming}
		}},
		{name: "archiving", reason: "sandbox is archived or being archived", prepare: func(_ *testing.T, _ *sandboxstore.Store, sandbox *domain.Sandbox, _ *sandboxes.WorkspaceDepsPruner) {
			sandbox.Archive = &domain.SandboxArchive{State: domain.SandboxArchiveStateArchiving}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, sandbox, pruner := newWorkspaceDepsSandbox(t)
			writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
			writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
			test.prepare(t, store, sandbox, pruner)
			if err := store.UpdateSandbox(ctx, sandbox); err != nil {
				t.Fatal(err)
			}

			result, err := pruner.Prune(ctx, sandbox.Summary.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result.SkipReason != test.reason || result.Removed != 0 {
				t.Fatalf("result = %#v, want skip reason %q", result, test.reason)
			}
			assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)
			current, err := store.GetSandbox(ctx, sandbox.Summary.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.WorkspaceDepsPrune != nil {
				t.Fatalf("prune record = %#v, want none", current.WorkspaceDepsPrune)
			}
		})
	}
}

func TestWorkspaceDepsPrunerReturnsBindingLookupError(t *testing.T) {
	_, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	lookupErr := errors.New("database is locked")
	pruner.Bindings = workspaceDepsTestBindings{err: lookupErr}

	if _, err := pruner.Prune(context.Background(), sandbox.Summary.ID); !errors.Is(err, lookupErr) {
		t.Fatalf("Prune error = %v, want %v", err, lookupErr)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)
}

func TestWorkspaceDepsPrunerDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	pruner.DryRun = true
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	before, err := os.ReadFile(filepath.Join(store.SandboxDir(sandbox.Summary.ID), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || !slices.Equal(result.Dirs, []string{"ui/node_modules"}) || result.Removed != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)
	after, err := os.ReadFile(filepath.Join(store.SandboxDir(sandbox.Summary.ID), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry run changed sandbox metadata")
	}
}

func TestWorkspaceDepsPrunerPrunesAgainOnlyAfterANewStop(t *testing.T) {
	ctx := context.Background()
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	if result, err := pruner.Prune(ctx, sandbox.Summary.ID); err != nil || result.Removed != 1 {
		t.Fatalf("first prune = %#v/%v", result, err)
	}

	// Dependencies reinstalled without a new stop are not this stop's to remove.
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	result, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil || result.SkipReason != "dependencies were already pruned for this stop" {
		t.Fatalf("repeated prune = %#v/%v", result, err)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)

	// A resume followed by another stop records a later stop time.
	vmState, err := store.GetVMState(sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	vmState.StartedAt = workspaceDepsTestNow.Add(-40 * time.Second)
	vmState.StoppedAt = workspaceDepsTestNow.Add(-10 * time.Second)
	if err := store.SaveVMState(sandbox.Summary.ID, vmState); err != nil {
		t.Fatal(err)
	}
	result, err = pruner.Prune(ctx, sandbox.Summary.ID)
	if err != nil || result.SkipReason != "" || result.Removed != 1 {
		t.Fatalf("prune after new stop = %#v/%v", result, err)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/package.json"}, []string{"ui/node_modules"})
	current, err := store.GetSandbox(ctx, sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.WorkspaceDepsPrune.StoppedAt.Equal(vmState.StoppedAt) {
		t.Fatalf("prune record stop = %s, want %s", current.WorkspaceDepsPrune.StoppedAt, vmState.StoppedAt)
	}
}

func TestWorkspaceDepsPrunerCancelledBeforeRemovalKeepsDirectories(t *testing.T) {
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	ctx, cancel := context.WithCancel(context.Background())
	// The clock is read once for the grace check and again for the start time of
	// the prune record, so cancelling on the second read interrupts the prune
	// after its intent is formed and before anything is removed.
	reads := 0
	pruner.Now = func() time.Time {
		reads++
		if reads == 2 {
			cancel()
		}
		return workspaceDepsTestNow
	}

	_, err := pruner.Prune(ctx, sandbox.Summary.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prune error = %v, want context.Canceled", err)
	}
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)
	current, getErr := store.GetSandbox(context.Background(), sandbox.Summary.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if record := current.WorkspaceDepsPrune; record != nil && record.State != domain.SandboxWorkspaceDepsPruneStatePruning {
		t.Fatalf("prune record = %#v, want absent or pruning", record)
	}
}

func TestWorkspaceDepsPrunerWorkerPrunesNotifiedSandbox(t *testing.T) {
	_, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	target := writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")

	// A notification that arrives before the worker starts is still handled.
	pruner.Notify(sandbox.Summary.ID)
	pruner.Start(context.Background())
	waitForWorkspaceDepsRemoval(t, target)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pruner.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	// Notifications after shutdown are dropped rather than queued forever.
	writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	pruner.Notify(sandbox.Summary.ID)
	time.Sleep(50 * time.Millisecond)
	assertWorkspaceDepsPaths(t, sandbox, []string{"ui/node_modules/pkg/index.js"}, nil)
}

func TestWorkspaceDepsPrunerWorkerWaitsForGrace(t *testing.T) {
	store, sandbox, pruner := newWorkspaceDepsSandbox(t)
	writeWorkspaceDepsFile(t, sandbox, "ui/package.json")
	target := writeWorkspaceDepsFile(t, sandbox, "ui/node_modules/pkg/index.js")
	// The grace period is measured from the recorded stop, so move the stop to
	// the real clock and let the worker use real time.
	stoppedAt := time.Now().UTC()
	vmState, err := store.GetVMState(sandbox.Summary.ID)
	if err != nil {
		t.Fatal(err)
	}
	vmState.StoppedAt = stoppedAt
	if err := store.SaveVMState(sandbox.Summary.ID, vmState); err != nil {
		t.Fatal(err)
	}
	pruner.Now = nil
	pruner.Grace = 300 * time.Millisecond
	pruner.Start(context.Background())
	t.Cleanup(func() { _ = pruner.Shutdown(context.Background()) })

	pruner.Notify(sandbox.Summary.ID)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("dependencies were removed inside the grace period: %v", err)
	}
	waitForWorkspaceDepsRemoval(t, target)
	if elapsed := time.Since(stoppedAt); elapsed < pruner.Grace {
		t.Fatalf("dependencies removed after %s, before the %s grace period", elapsed, pruner.Grace)
	}
}

func waitForWorkspaceDepsRemoval(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not removed", target)
}
