package runs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chaitin/agent-compose/pkg/config"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestDetachFollowerReconstructsResultAcrossChunks(t *testing.T) {
	var forwarded []domain.ExecChunk
	var persisted []uint64
	f := newDetachFollower(
		func(chunk domain.ExecChunk) { forwarded = append(forwarded, chunk) },
		func(offset uint64) error { persisted = append(persisted, offset); return nil },
	)

	// Split a record across chunk boundaries to exercise line buffering.
	f.consume(domain.ExecChunk{Text: `{"t":"out","s":"stdout","text":"hi\n","off`})
	f.consume(domain.ExecChunk{Text: `set":0}` + "\n" + `{"t":"out","s":"stderr","text":"err\n","offset":3}` + "\n" + `{"t":"ex`})
	f.consume(domain.ExecChunk{Text: `it","code":0,"reason":"completed"}` + "\n"})

	res := f.result(nil, context.Background())
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Result.Stdout != "hi\n" || res.Result.Stderr != "err\n" || res.Result.Output != "hi\nerr\n" {
		t.Fatalf("result = %+v", res.Result)
	}
	if res.Result.ExitCode != 0 || !res.Result.Success {
		t.Fatalf("exit = %+v", res.Result)
	}
	if f.cursor != 7 {
		t.Fatalf("cursor = %d, want 7", f.cursor)
	}
	if len(persisted) == 0 || persisted[len(persisted)-1] != 7 {
		t.Fatalf("persisted cursor = %v, want last 7", persisted)
	}
	if len(forwarded) != 2 {
		t.Fatalf("forwarded %d chunks, want 2", len(forwarded))
	}
	if forwarded[0].Stream != domain.StdioStdout || forwarded[1].Stream != domain.StdioStderr {
		t.Fatalf("forwarded streams = %v", forwarded)
	}
}

func TestDetachFollowerExitMapping(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantErr    bool
		wantStop   string
		wantCode   int
		wantCancel bool
	}{
		{"completed", `{"t":"exit","code":0,"reason":"completed"}`, false, "", 0, false},
		{"stopped user", `{"t":"exit","code":143,"reason":"stopped","stopReason":"user"}`, true, "user", 143, true},
		{"stopped timeout", `{"t":"exit","code":124,"reason":"stopped","stopReason":"timeout"}`, true, "timeout", 124, false},
		{"abnormal", `{"t":"exit","code":-1,"reason":"abnormal"}`, true, "", -1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDetachFollower(nil, nil)
			f.consume(domain.ExecChunk{Text: tc.line + "\n"})
			res := f.result(nil, context.Background())
			if (res.Err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", res.Err, tc.wantErr)
			}
			if res.StopReason != tc.wantStop {
				t.Fatalf("stop reason = %q, want %q", res.StopReason, tc.wantStop)
			}
			if res.Result.ExitCode != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", res.Result.ExitCode, tc.wantCode)
			}
			if tc.wantCancel && !strings.Contains(res.Err.Error(), "stopped") {
				t.Fatalf("expected stopped error, got %v", res.Err)
			}
		})
	}
}

func TestDetachFollowerDisconnectedWithoutExit(t *testing.T) {
	f := newDetachFollower(nil, nil)
	attachCtx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunDisconnected)
	res := f.result(context.Canceled, attachCtx)
	if !errors.Is(res.Err, ErrRunDisconnected) {
		t.Fatalf("err = %v, want ErrRunDisconnected", res.Err)
	}
}

func TestDetachStopReason(t *testing.T) {
	if got := detachStopReason(context.Background()); got != "user" {
		t.Fatalf("got %q, want user", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	_ = cancel
	<-ctx.Done()
	if got := detachStopReason(ctx); got != "timeout" {
		t.Fatalf("got %q, want timeout", got)
	}
}

func TestDetachIsShutdown(t *testing.T) {
	if detachIsShutdown(context.Background()) {
		t.Fatal("background context reported as shutdown")
	}
	// Fresh-run path: the exec context is cancelled with ErrRunDisconnected when
	// the daemon shutdown signal fires.
	execCtx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunDisconnected)
	if !detachIsShutdown(execCtx) {
		t.Fatal("ErrRunDisconnected cause not reported as shutdown")
	}
	// Re-attach path: the run context stays alive and only carries the fired
	// shutdown signal as a value.
	shutdown := make(chan struct{})
	close(shutdown)
	if !detachIsShutdown(WithDaemonShutdown(context.Background(), shutdown)) {
		t.Fatal("fired shutdown signal not reported as shutdown")
	}
}

// detachStopFakeRuntime records stop requests and marker terminations, and blocks
// the attach exec until its context is cancelled so followDetachedCommand stays
// in the follow loop long enough to exercise the stop/shutdown paths.
type detachStopFakeRuntime struct {
	mu               sync.Mutex
	stopCalls        int
	terminated       bool
	terminatedMarker string
	stopErr          error
	terminateErr     error
}

func (r *detachStopFakeRuntime) ExecStream(ctx context.Context, _ *domain.Sandbox, _ domain.VMState, spec domain.ExecSpec, _ domain.ExecStreamWriter) (domain.ExecResult, error) {
	if len(spec.Args) > 0 && spec.Args[0] == "stop" {
		r.mu.Lock()
		r.stopCalls++
		r.mu.Unlock()
		return domain.ExecResult{}, r.stopErr
	}
	if len(spec.Args) > 0 && spec.Args[0] == "attach" {
		<-ctx.Done()
		return domain.ExecResult{}, ctx.Err()
	}
	return domain.ExecResult{}, nil
}

func (r *detachStopFakeRuntime) TerminateProcessTree(_ context.Context, _ *domain.Sandbox, _ domain.VMState, marker string) error {
	r.mu.Lock()
	r.terminated = true
	r.terminatedMarker = marker
	r.mu.Unlock()
	return r.terminateErr
}

func (r *detachStopFakeRuntime) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCalls
}

func (r *detachStopFakeRuntime) termination() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminated, r.terminatedMarker
}

func TestFollowDetachedCommandDoesNotStopOnShutdown(t *testing.T) {
	shutdown := make(chan struct{})
	runCtx := WithDaemonShutdown(context.Background(), shutdown)
	execCtx, cancel := runExecContext(runCtx)
	defer cancel()

	rt := &detachStopFakeRuntime{}
	opts := detachedExecutionOptions{
		Config:      &config.Config{},
		Runtime:     rt,
		Sandbox:     &domain.Sandbox{},
		VMState:     domain.VMState{},
		ExecutionID: "exec-1",
	}

	done := make(chan detachedExecutionResult, 1)
	go func() { done <- followDetachedCommand(execCtx, opts) }()

	close(shutdown) // daemon shutdown

	result := <-done
	if !errors.Is(result.Err, ErrRunDisconnected) {
		t.Fatalf("err = %v, want ErrRunDisconnected", result.Err)
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("stop was sent %d times on shutdown, want 0", n)
	}
}

func TestFollowDetachedCommandTerminatesTreeWhenStopFails(t *testing.T) {
	rt := &detachStopFakeRuntime{stopErr: errors.New("stop failed")}
	ctx, cancel := context.WithCancel(context.Background())
	opts := detachedExecutionOptions{
		Config:      &config.Config{},
		Runtime:     rt,
		Sandbox:     &domain.Sandbox{},
		VMState:     domain.VMState{},
		ExecutionID: "exec-1",
	}

	done := make(chan detachedExecutionResult, 1)
	go func() { done <- followDetachedCommand(ctx, opts) }()

	cancel() // user stop

	result := <-done
	if result.Err == nil || !strings.Contains(result.Err.Error(), "stop request failed") {
		t.Fatalf("err = %v, want stop-failed error", result.Err)
	}
	terminated, marker := rt.termination()
	if !terminated {
		t.Fatal("TerminateProcessTree was not called after stop failed")
	}
	if marker != "exec-1" {
		t.Fatalf("terminated marker = %q, want exec-1", marker)
	}
}
