package runs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/storage/sandboxstore"
)

func TestReattachDetachedRunIgnoresNonDetachRun(t *testing.T) {
	controller := &Controller{}
	run := domain.ProjectRunRecord{RunID: "run-1", Detach: false}
	got, err := controller.ReattachDetachedRun(context.Background(), run)
	if err != nil {
		t.Fatalf("ReattachDetachedRun returned error: %v", err)
	}
	if got.RunID != "run-1" {
		t.Fatalf("run changed: %+v", got)
	}
}

func TestReattachDetachedRunMarksFailedWhenSandboxNotRunning(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := &config.Config{
		DataRoot:      root,
		SandboxRoot:   filepath.Join(root, "sandboxes"),
		RuntimeDriver: driverpkg.RuntimeDriverK8s,
		DefaultImage:  "guest:latest",
	}
	store, err := sandboxstore.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig returned error: %v", err)
	}
	sandbox, err := store.CreateSandbox(ctx, "re-attach", "", driverpkg.RuntimeDriverK8s, "guest:latest", "", domain.SandboxTypeScript, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	configDB := &fakeControllerStore{runs: map[string]domain.ProjectRunRecord{}}
	run := domain.ProjectRunRecord{RunID: "run-1", ProjectID: "project-1", SandboxID: sandbox.Summary.ID, Detach: true, ExecutionID: "exec-1", Status: domain.ProjectRunStatusRunning, CleanupPolicy: domain.ProjectRunCleanupKeepRunning}
	if _, err := configDB.CreateProjectRun(ctx, run); err != nil {
		t.Fatalf("CreateProjectRun returned error: %v", err)
	}
	controller := &Controller{
		config:   cfg,
		store:    store,
		configDB: configDB,
		runtime:  func(*domain.Sandbox) (Runtime, error) { return &fakeControllerRuntime{}, nil },
	}
	// The sandbox is still pending, so re-attach must fail the run rather than
	// hang on a follow.
	got, err := controller.ReattachDetachedRun(ctx, run)
	if err != nil {
		t.Fatalf("ReattachDetachedRun returned error: %v", err)
	}
	if got.Status != domain.ProjectRunStatusFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
}

type reattachFakeRuntime struct {
	fakeControllerRuntime
	records []string
}

func (r *reattachFakeRuntime) ExecStream(ctx context.Context, session *domain.Sandbox, vmState domain.VMState, spec domain.ExecSpec, writer domain.ExecStreamWriter) (domain.ExecResult, error) {
	if len(spec.Args) > 0 && spec.Args[0] == "attach" {
		for _, record := range r.records {
			writer(domain.ExecChunk{Text: record + "\n"})
		}
		return domain.ExecResult{}, nil
	}
	return r.fakeControllerRuntime.ExecStream(ctx, session, vmState, spec, writer)
}

func TestReattachDetachedRunFollowsJournalAndCompletes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := &config.Config{
		DataRoot:           root,
		SandboxRoot:        filepath.Join(root, "sandboxes"),
		RuntimeDriver:      driverpkg.RuntimeDriverK8s,
		DefaultImage:       "guest:latest",
		GuestWorkspacePath: "/workspace",
		GuestStateRoot:     "/state",
		GuestHomePath:      "/root",
	}
	store, err := sandboxstore.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig returned error: %v", err)
	}
	sandbox, err := store.CreateSandbox(ctx, "re-attach", "", driverpkg.RuntimeDriverK8s, "guest:latest", "", domain.SandboxTypeScript, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandbox.Summary.VMStatus = domain.VMStatusRunning
	if err := store.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpdateSandbox returned error: %v", err)
	}
	if err := store.SaveVMState(sandbox.Summary.ID, domain.VMState{Driver: driverpkg.RuntimeDriverK8s, BoxID: sandbox.Summary.ID}); err != nil {
		t.Fatalf("SaveVMState returned error: %v", err)
	}
	configDB := &fakeControllerStore{runs: map[string]domain.ProjectRunRecord{}}
	run := domain.ProjectRunRecord{RunID: "run-1", ProjectID: "project-1", SandboxID: sandbox.Summary.ID, Detach: true, ExecutionID: "exec-1", Status: domain.ProjectRunStatusRunning, CleanupPolicy: domain.ProjectRunCleanupKeepRunning}
	if _, err := configDB.CreateProjectRun(ctx, run); err != nil {
		t.Fatalf("CreateProjectRun returned error: %v", err)
	}
	// Cursor 5 means the daemon already consumed offsets 0..5; the tail resumes
	// at offset 5 without duplicating earlier output.
	controller := &Controller{
		config:   cfg,
		store:    store,
		configDB: configDB,
		runtime: func(*domain.Sandbox) (Runtime, error) {
			return &reattachFakeRuntime{records: []string{
				`{"t":"out","s":"stdout","text":"tail\n","offset":5}`,
				`{"t":"exit","code":0,"reason":"completed"}`,
			}}, nil
		},
	}
	got, err := controller.ReattachDetachedRun(ctx, run)
	if err != nil {
		t.Fatalf("ReattachDetachedRun returned error: %v", err)
	}
	if got.Status != domain.ProjectRunStatusSucceeded || got.ExitCode != 0 {
		t.Fatalf("run = %+v, want succeeded", got)
	}
	// Re-attach must append to the same transcript the live run was writing, not
	// a fresh output.txt, so pre-restart output stays in the run log.
	if filepath.Base(got.LogsPath) != "transcript.txt" {
		t.Fatalf("LogsPath = %q, want transcript.txt", got.LogsPath)
	}
}
