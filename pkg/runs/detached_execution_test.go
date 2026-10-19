package runs

import (
	"context"
	"errors"
	"strings"
	"testing"

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
