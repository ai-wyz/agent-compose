package runs

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/chaitin/agent-compose/pkg/execution"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// ReattachDetachedRun resumes following a detached run left running by a
// daemon shutdown or restart. It re-attaches to the guest journal, appends any
// output that arrived while the daemon was gone, and completes the run when the
// task exits. A run whose sandbox is no longer running is marked failed.
func (c *Controller) ReattachDetachedRun(ctx context.Context, run domain.ProjectRunRecord) (domain.ProjectRunRecord, error) {
	if !run.Detach || run.ExecutionID == "" {
		return run, nil
	}
	sandbox, err := c.store.GetSandbox(ctx, run.SandboxID)
	if err != nil {
		return run, err
	}
	if sandbox.Summary.VMStatus != domain.VMStatusRunning {
		transition := TransitionRequest{
			RunID:        run.RunID,
			SandboxID:    run.SandboxID,
			ExitCode:     1,
			Output:       run.Output,
			ArtifactsDir: run.ArtifactsDir,
			LogsPath:     run.LogsPath,
			Error:        "detached execution sandbox is no longer running",
		}
		return c.completeProjectRunError(ctx, ctx, transition, errors.New(transition.Error))
	}
	vmState, err := c.store.GetVMState(sandbox.Summary.ID)
	if err != nil {
		return run, err
	}
	runtime, err := c.runtime(sandbox)
	if err != nil {
		return run, err
	}

	runCtx := ctx
	if !run.Deadline.IsZero() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithDeadline(ctx, run.Deadline)
		defer cancel()
	}
	runCtx = WithDaemonShutdown(runCtx, daemonShutdownFromContext(ctx))

	artifactsDir := projectRunCommandArtifactsDir(run, sandbox)
	logsPath := run.LogsPath
	if logsPath == "" {
		logsPath = filepath.Join(artifactsDir, "output.txt")
	}
	var sendErr error
	writer := func(chunk domain.ExecChunk) {
		if sendErr != nil {
			return
		}
		filtered, visible := execution.FilterCommandStreamChunk(chunk)
		if !visible {
			return
		}
		offset, err := appendProjectRunLogChunk(logsPath, filtered)
		if err != nil {
			sendErr = err
			return
		}
		c.publishRunLogChunk(run.RunID, filtered, offset)
	}

	env := execution.BuildSandboxExecEnv(runCtx, c.config, sandbox, c.config.GuestHomePath)
	env["AGENT_COMPOSE_RUN_ID"] = run.RunID
	env["AGENT_COMPOSE_PROJECT_ID"] = run.ProjectID

	result := followDetachedCommand(runCtx, detachedExecutionOptions{
		Config:      c.config,
		Runtime:     runtime,
		Sandbox:     sandbox,
		VMState:     vmState,
		ExecutionID: run.ExecutionID,
		Env:         env,
		Cursor:      readDetachCursor(artifactsDir),
		Writer:      writer,
		PersistCursor: func(offset uint64) error {
			return writeDetachCursor(artifactsDir, offset)
		},
	})
	if errors.Is(result.Err, ErrRunDisconnected) {
		// Shut down again while following; leave the run running for the next
		// reconciliation.
		return run, nil
	}
	if sendErr != nil {
		result.Err = errors.Join(result.Err, sendErr)
	}
	if pullErr := execution.SyncGuestDirToHost(ctx, filepath.Join(c.config.GuestStateRoot, "runs", run.RunID), artifactsDir, guestDirReaderFor(runtime, sandbox, vmState)); pullErr != nil {
		result.Err = errors.Join(result.Err, pullErr)
	}

	transition := transitionFromCommandResult(run, sandbox, commandOutcome{Result: result.Result, Err: result.Err})
	transition.LogsPath = logsPath
	if result.Err != nil {
		return c.completeProjectRunError(ctx, runCtx, transition, result.Err)
	}
	transition.Status = domain.ProjectRunStatusSucceeded
	return c.completeProjectRun(ctx, transition)
}

// daemonShutdownFromContext extracts the shutdown signal carried by ctx (if
// any) so a re-attach can disconnect on the next shutdown without terminating
// the task.
func daemonShutdownFromContext(ctx context.Context) <-chan struct{} {
	shutdown, _ := DaemonShutdown(ctx)
	return shutdown
}
