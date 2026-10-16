package adapters

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/storage/sandboxstore"
)

// refusingPreflightRuntime refuses the preflight and counts the calls that
// reached EnsureSandbox. Runtime state must not be created after the runtime
// itself said it cannot start, so the count is the observable fact this test
// asserts on rather than the fake's return value.
type refusingPreflightRuntime struct {
	preflightErr error
	ensureCalls  *int
}

func (r refusingPreflightRuntime) PreflightSandboxRuntime(context.Context, *domain.Sandbox, domain.VMState) error {
	return r.preflightErr
}

func (r refusingPreflightRuntime) EnsureSandbox(context.Context, *domain.Sandbox, domain.VMState, domain.ProxyState) (domain.SandboxVMInfo, error) {
	*r.ensureCalls++
	return domain.SandboxVMInfo{}, nil
}

func (r refusingPreflightRuntime) StopSandbox(context.Context, *domain.Sandbox, domain.VMState) (bool, error) {
	return false, nil
}

func (r refusingPreflightRuntime) RemoveSandbox(context.Context, *domain.Sandbox, domain.VMState) error {
	return nil
}

func (r refusingPreflightRuntime) Exec(context.Context, *domain.Sandbox, domain.VMState, domain.ExecSpec) (domain.ExecResult, error) {
	return domain.ExecResult{}, nil
}

func (r refusingPreflightRuntime) ExecStream(context.Context, *domain.Sandbox, domain.VMState, domain.ExecSpec, domain.ExecStreamWriter) (domain.ExecResult, error) {
	return domain.ExecResult{}, nil
}

type refusingPreflightRuntimeProvider struct {
	runtime SandboxRuntime
}

func (p refusingPreflightRuntimeProvider) ForDriver(string) (SandboxRuntime, error) {
	return p.runtime, nil
}

func (p refusingPreflightRuntimeProvider) ForSession(*domain.Sandbox) (SandboxRuntime, error) {
	return p.runtime, nil
}

// TestSandboxDriverPreflightFailureStopsTheStartBeforeRuntimeState pins the
// order StartSandboxVM promises: a runtime that reports it is unusable fails the
// sandbox with a classified reason and no runtime is created. Reversing the
// order, or dropping the preflight entirely, leaves an unstartable sandbox with
// partially created state for cleanup to chase - the behavior the test round
// observed when no cluster was configured.
func TestSandboxDriverPreflightFailureStopsTheStartBeforeRuntimeState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := &appconfig.Config{
		DataRoot:             root,
		SandboxRoot:          filepath.Join(root, "sandboxes"),
		RuntimeDriver:        driverpkg.RuntimeDriverBoxlite,
		BoxliteHome:          filepath.Join(root, "boxlite"),
		DefaultImage:         "guest:latest",
		GuestWorkspacePath:   "/workspace",
		JupyterGuestPort:     8888,
		JupyterProxyBasePath: "/agent-compose/session",
		SandboxStartTimeout:  2 * time.Second,
	}
	store, err := sandboxstore.NewWithConfig(config)
	if err != nil {
		t.Fatalf("NewWithConfig returned error: %v", err)
	}
	session, err := store.CreateSandbox(ctx, "k8s preflight", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	ensureCalls := 0
	provider := refusingPreflightRuntimeProvider{runtime: refusingPreflightRuntime{
		preflightErr: domain.ClassifyError(domain.ErrFailedPrecondition, "kubernetes runtime is not usable", errors.New("no kubeconfig")),
		ensureCalls:  &ensureCalls,
	}}
	driver := newTestSandboxDriver(config, store, nil, provider)

	err = driver.StartSandboxVM(ctx, session)
	if !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Fatalf("StartSandboxVM error = %v, want a failed precondition", err)
	}
	if ensureCalls != 0 {
		t.Fatalf("EnsureSandbox called %d times after the preflight refused, want 0", ensureCalls)
	}
	vmState, err := store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.LastError == "" {
		t.Fatal("GetVMState LastError is empty, want the preflight failure recorded for the sandbox")
	}
}
