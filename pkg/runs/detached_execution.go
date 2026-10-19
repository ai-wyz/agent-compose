package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/execution"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// ErrRunDisconnected reports that an attach was dropped because the daemon
// shut down while the detached task keeps running. The run must be left in its
// running state for a later reconciliation to re-attach, not marked terminal.
var ErrRunDisconnected = errors.New("run disconnected: task still running in guest")

// detachStopTimeout bounds how long the daemon waits for the `stop` request to
// reach the supervisor when terminating a detached task.
const detachStopTimeout = 30 * time.Second

// detachedExecutionOptions bundles the inputs for a detached command run.
type detachedExecutionOptions struct {
	Config      *appconfig.Config
	Runtime     Runtime
	Sandbox     *domain.Sandbox
	VMState     domain.VMState
	ExecutionID string
	// TaskArgs is the guest CLI subcommand plus flags (e.g. ["exec",
	// "--request-file", ...]); the supervisor re-enters the CLI with them.
	TaskArgs []string
	Env      map[string]string
	// Cursor is the journal offset to resume from (0 for a fresh run).
	Cursor uint64
	// Writer forwards task output chunks to the run log; may be nil.
	Writer func(domain.ExecChunk)
	// PersistCursor records the highest consumed journal offset so a restart
	// resumes from there without duplicating output.
	PersistCursor func(uint64) error
}

// detachedExecutionResult is the reconstructed outcome of a detached run.
type detachedExecutionResult struct {
	Result     domain.ExecResult
	StopReason string
	Err        error
}

// executeDetachedCommand runs a guest task behind the detach supervisor and
// follows its journal until the task ends. ctx is the run context: cancelled
// only by StopActiveRun or a timeout. On ctx cancellation it sends a stop
// request to the supervisor and waits for the exit record; on daemon shutdown
// (the signal attached with WithDaemonShutdown) it disconnects the follower
// and returns ErrRunDisconnected without terminating the task.
func executeDetachedCommand(ctx context.Context, opts detachedExecutionOptions) detachedExecutionResult {
	if result := startDetachedCommand(ctx, opts); result.Err != nil {
		return result
	}
	return followDetachedCommand(ctx, opts)
}

// startDetachedCommand launches the supervisor in the guest and waits for its
// ready record (the start exec blocks until the task is detached).
func startDetachedCommand(ctx context.Context, opts detachedExecutionOptions) detachedExecutionResult {
	startSpec := execution.BuildDetachStartSpec(opts.Config, opts.ExecutionID, opts.TaskArgs)
	startSpec.Env = opts.Env
	if _, err := opts.Runtime.ExecStream(ctx, opts.Sandbox, opts.VMState, startSpec, nil); err != nil {
		if ctx.Err() != nil {
			return detachedExecutionResult{Err: ctx.Err()}
		}
		return detachedExecutionResult{Err: fmt.Errorf("start detached execution: %w", err)}
	}
	return detachedExecutionResult{}
}

// followDetachedCommand attaches to a detached task and follows its journal to
// completion. It assumes the task is already running (startDetachedCommand, or
// an earlier daemon that is now gone).
func followDetachedCommand(ctx context.Context, opts detachedExecutionOptions) detachedExecutionResult {
	follower := newDetachFollower(opts.Writer, opts.PersistCursor)
	attachCtx, cancelAttach := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelAttach(nil)
	attachStop := detachAttachShutdownWatcher(attachCtx, cancelAttach, ctx)

	attachSpec := execution.BuildDetachAttachSpec(opts.Config, opts.ExecutionID, opts.Cursor)
	attachSpec.Env = opts.Env

	outcome := make(chan detachedExecutionResult, 1)
	go func() {
		defer attachStop()
		_, err := opts.Runtime.ExecStream(attachCtx, opts.Sandbox, opts.VMState, attachSpec, follower.consume)
		outcome <- follower.result(err, attachCtx)
	}()

	select {
	case <-ctx.Done():
		reason := detachStopReason(ctx)
		stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), detachStopTimeout)
		defer cancelStop()
		stopSpec := execution.BuildDetachStopSpec(opts.Config, opts.ExecutionID, reason)
		stopSpec.Env = opts.Env
		if _, stopErr := opts.Runtime.ExecStream(stopCtx, opts.Sandbox, opts.VMState, stopSpec, nil); stopErr != nil {
			// The stop request could not reach the supervisor. Drop the follower
			// and report the failure; the caller falls back to marker cleanup.
			cancelAttach(fmt.Errorf("detached execution stop request failed: %w", stopErr))
		}
		result := <-outcome
		result.StopReason = reason
		return result
	case result := <-outcome:
		return result
	}
}

// detachAttachShutdownWatcher cancels attachCtx when the daemon shutdown signal
// fires, and returns a stop function that ends the watcher goroutine.
func detachAttachShutdownWatcher(attachCtx context.Context, cancel context.CancelCauseFunc, runCtx context.Context) func() {
	shutdown, ok := DaemonShutdown(runCtx)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-shutdown:
			cancel(ErrRunDisconnected)
		case <-attachCtx.Done():
		}
	}()
	return func() {
		cancel(nil)
		<-done
	}
}

// detachStopReason maps a cancelled run context to the stop reason to record.
func detachStopReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return execution.DetachStopReasonTimeout
	}
	return execution.DetachStopReasonUser
}

// executeDetachedProjectRunCommand runs a project-run command task behind the
// detach supervisor and returns its reconstructed result. It records the
// execution id and detach decision on the run before starting, and persists
// the journal cursor as it follows the task so a restart resumes cleanly.
func (c *Controller) executeDetachedProjectRunCommand(ctx context.Context, runtime Runtime, sandbox *domain.Sandbox, vmState domain.VMState, run domain.ProjectRunRecord, guestRequestPath string, writer func(domain.ExecChunk)) (domain.ExecResult, error) {
	executionID := newDetachExecutionID(run)
	if err := c.persistDetachedRunFields(ctx, run, executionID); err != nil {
		return domain.ExecResult{}, fmt.Errorf("persist detached run fields: %w", err)
	}
	artifactsDir := projectRunCommandArtifactsDir(run, sandbox)
	taskArgs := execution.RuntimeCommandCLIArgs(c.config, guestRequestPath, c.config.GuestHomePath)
	env := execution.BuildSandboxExecEnv(ctx, c.config, sandbox, c.config.GuestHomePath)
	env["AGENT_COMPOSE_RUN_ID"] = run.RunID
	env["AGENT_COMPOSE_PROJECT_ID"] = run.ProjectID

	result := executeDetachedCommand(ctx, detachedExecutionOptions{
		Config:      c.config,
		Runtime:     runtime,
		Sandbox:     sandbox,
		VMState:     vmState,
		ExecutionID: executionID,
		TaskArgs:    taskArgs,
		Env:         env,
		Cursor:      readDetachCursor(artifactsDir),
		Writer:      writer,
		PersistCursor: func(offset uint64) error {
			return writeDetachCursor(artifactsDir, offset)
		},
	})
	return result.Result, result.Err
}

// detachFollower parses the NDJSON records emitted by `attach` and reconstructs
// the task result while forwarding output chunks to the run log.
type detachFollower struct {
	writer  func(domain.ExecChunk)
	persist func(uint64) error
	sendErr error

	pending strings.Builder
	exit    *execution.DetachAttachRecord

	stdout strings.Builder
	stderr strings.Builder
	output strings.Builder
	cursor uint64
}

func newDetachFollower(writer func(domain.ExecChunk), persist func(uint64) error) *detachFollower {
	return &detachFollower{writer: writer, persist: persist}
}

func (f *detachFollower) consume(chunk domain.ExecChunk) {
	if f.sendErr != nil || f.exit != nil {
		return
	}
	f.pending.WriteString(chunk.Text)
	for {
		line, rest, ok := strings.Cut(f.pending.String(), "\n")
		if !ok {
			break
		}
		f.pending.Reset()
		f.pending.WriteString(rest)
		f.handleLine(line)
	}
}

func (f *detachFollower) handleLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	record, err := execution.ParseDetachAttachLine(line)
	if err != nil {
		f.sendErr = err
		return
	}
	switch {
	case record.IsExit():
		f.exit = &record
	case record.IsOut():
		f.appendOut(record)
	case record.Type == "gap":
		if record.To > f.cursor {
			f.cursor = record.To
			f.persistCursor()
		}
		f.forward(domain.ExecChunk{Stream: domain.StdioStderr, Text: fmt.Sprintf("\n[detach journal gap: %d..%d]\n", record.From, record.To)})
	}
}

func (f *detachFollower) appendOut(record execution.DetachAttachRecord) {
	stream := domain.StdioStream(record.Stream)
	if stream != domain.StdioStdout && stream != domain.StdioStderr {
		stream = domain.StdioStdout
	}
	if stream == domain.StdioStdout {
		f.stdout.WriteString(record.Text)
	} else {
		f.stderr.WriteString(record.Text)
	}
	f.output.WriteString(record.Text)
	if end := record.Offset + uint64(len(record.Text)); end > f.cursor {
		f.cursor = end
	}
	f.persistCursor()
	f.forward(domain.ExecChunk{Stream: stream, Text: record.Text})
}

func (f *detachFollower) persistCursor() {
	if f.persist == nil || f.sendErr != nil {
		return
	}
	if err := f.persist(f.cursor); err != nil {
		f.sendErr = err
	}
}

func (f *detachFollower) forward(chunk domain.ExecChunk) {
	if f.writer != nil {
		f.writer(chunk)
	}
}

func (f *detachFollower) result(execErr error, attachCtx context.Context) detachedExecutionResult {
	if f.sendErr != nil {
		execErr = errors.Join(execErr, f.sendErr)
	}
	result := domain.ExecResult{
		Stdout: f.stdout.String(),
		Stderr: f.stderr.String(),
		Output: f.output.String(),
	}
	if f.exit == nil {
		result.ExitCode = -1
		if execErr != nil {
			if cause := context.Cause(attachCtx); errors.Is(cause, ErrRunDisconnected) {
				return detachedExecutionResult{Result: result, Err: ErrRunDisconnected}
			}
			return detachedExecutionResult{Result: result, Err: execErr}
		}
		return detachedExecutionResult{Result: result, Err: errors.New("detached execution ended without an exit record")}
	}
	exit := execution.DetachExitResultFor(*f.exit)
	mapped, exitErr := execution.ExecResultFromDetachExit(exit)
	mapped.Stdout = result.Stdout
	mapped.Stderr = result.Stderr
	mapped.Output = result.Output
	if exitErr == nil {
		mapped.Success = mapped.ExitCode == 0
	}
	stopReason := ""
	if exit.Reason == execution.DetachExitStopped {
		stopReason = exit.StopReason
	}
	return detachedExecutionResult{Result: mapped, StopReason: stopReason, Err: exitErr}
}
