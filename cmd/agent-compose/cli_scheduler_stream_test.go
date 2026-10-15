package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
)

func TestIntegrationSchedulerStreamsOutputBeforeCompletion(t *testing.T) {
	composePath := writeComposeFile(t, t.TempDir(), `
name: scheduler-stream-output
agents:
  reviewer:
    provider: codex
    scheduler:
      triggers:
        - name: nightly
          cron: "0 2 * * *"
          prompt: review
`)
	tests := []struct {
		name   string
		args   []string
		marker string
		stub   projectServiceStub
	}{
		{
			name:   "runs",
			args:   []string{"scheduler", "runs"},
			marker: "run-stream",
			stub: projectServiceStub{streamSchedulerRuns: func(ctx context.Context, _ *connect.Request[agentcomposev2.StreamSchedulerRunsRequest], stream *connect.ServerStream[agentcomposev2.StreamSchedulerRunsResponse]) error {
				if err := stream.Send(&agentcomposev2.StreamSchedulerRunsResponse{Runs: []*agentcomposev2.SchedulerRun{{RunId: "run-stream-first", AgentName: "reviewer", TriggerId: "nightly", Status: agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_RUNNING}}}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}},
		},
		{
			name:   "logs",
			args:   []string{"scheduler", "logs"},
			marker: "first-stream-event",
			stub: projectServiceStub{streamSchedulerEvents: func(ctx context.Context, _ *connect.Request[agentcomposev2.StreamProjectSchedulerEventsRequest], stream *connect.ServerStream[agentcomposev2.StreamProjectSchedulerEventsResponse]) error {
				if err := stream.Send(&agentcomposev2.StreamProjectSchedulerEventsResponse{Events: []*agentcomposev2.SchedulerEvent{{Id: "event-first", AgentName: "reviewer", TriggerId: "nightly", Type: "scheduler.log", Message: "first-stream-event"}}}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stub := test.stub
			stub.getProject = func(_ context.Context, req *connect.Request[agentcomposev2.GetProjectRequest]) (*connect.Response[agentcomposev2.GetProjectResponse], error) {
				return connect.NewResponse(&agentcomposev2.GetProjectResponse{Project: testCLIProject(req.Msg.GetProject().GetProjectId(), "scheduler-stream-output", composePath)}), nil
			}
			server := newComposeServiceStubServer(t, composeServiceStubs{project: stub})
			defer server.Close()
			out := newMarkerWriter(test.marker)
			var stderr bytes.Buffer
			done := make(chan int, 1)
			args := append(append([]string(nil), test.args...), "--host", server.URL, "--file", composePath)
			go func() { done <- executeCLI(ctx, out, &stderr, args, nil) }()
			select {
			case <-out.seen:
			case code := <-done:
				t.Fatalf("command completed before streaming the first item: code=%d stderr=%q", code, stderr.String())
			case <-ctx.Done():
				t.Fatalf("timed out waiting for streamed output: %v", ctx.Err())
			}
			select {
			case code := <-done:
				t.Fatalf("command completed before the server stream ended: code=%d stderr=%q", code, stderr.String())
			default:
			}
			cancel()
			<-done
		})
	}
}

func TestSchedulerRunsTextWriterRendersTriggerLabel(t *testing.T) {
	t.Parallel()

	// A declarative trigger id is a stable 64-character digest, so the declared
	// name is the only readable part. A script trigger id is already the
	// registered name.
	const declarativeTriggerID = "8fdd8df0d7074f93a173968ccf97aa960e242330c4d0af48281a38c4f16ff122"

	tests := []struct {
		name string
		run  composeSchedulerRunItem
		want string
	}{
		{
			name: "declared name is shown with its short id",
			run: schedulerRuntimeRunItem("scheduler-reviewer", &agentcomposev2.SchedulerRun{
				RunId: "run-1", AgentName: "reviewer", TriggerId: declarativeTriggerID, TriggerName: "hourly-status",
				Status: agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_SUCCEEDED,
			}),
			want: "hourly-status (8fdd8df0d707)",
		},
		{
			name: "script trigger keeps its readable id",
			run:  composeSchedulerRunItem{RunShortID: "run-2", AgentName: "reviewer", TriggerID: "alertmanager-webhook", Status: "succeeded"},
			want: "alertmanager-webhook",
		},
		{
			name: "name equal to the id is not repeated",
			run:  composeSchedulerRunItem{RunShortID: "run-3", AgentName: "reviewer", TriggerID: "new-one", TriggerName: "new-one", Status: "succeeded"},
			want: "new-one",
		},
		{
			name: "trigger no longer declared falls back to the id",
			run:  composeSchedulerRunItem{RunShortID: "run-4", AgentName: "reviewer", TriggerID: declarativeTriggerID, Status: "skipped"},
			want: declarativeTriggerID,
		},
		{
			name: "missing trigger id stays a placeholder",
			run:  composeSchedulerRunItem{RunShortID: "run-5", AgentName: "reviewer", Status: "skipped"},
			want: "-",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			writer := newSchedulerRunsTextStreamWriter(&out)
			if err := writer.Write([]composeSchedulerRunItem{test.run}); err != nil {
				t.Fatalf("write scheduler run: %v", err)
			}
			if err := writer.Finish(); err != nil {
				t.Fatalf("finish scheduler runs: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("output = %q, want a header and one run row", out.String())
			}
			if got := schedulerRunRowColumn(t, lines[1], 2); got != test.want {
				t.Fatalf("TRIGGER column = %q, want %q (row %q)", got, test.want, lines[1])
			}
		})
	}
}

// schedulerRunRowColumn splits a tabwriter-padded run row so a rendered column
// can be compared exactly.
func schedulerRunRowColumn(t *testing.T, row string, index int) string {
	t.Helper()
	columns := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(row), -1)
	if len(columns) != 7 {
		t.Fatalf("row = %q, want 7 columns", row)
	}
	return columns[index]
}

func TestSchedulerJSONStreamWriterRejectsNonObjectPrefix(t *testing.T) {
	for name, prefix := range map[string]any{
		"nil":    nil,
		"string": "prefix",
		"slice":  []string{"prefix"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newSchedulerJSONStreamWriter[string](io.Discard, prefix, "items"); err == nil || !strings.Contains(err.Error(), "must encode a JSON object") {
				t.Fatalf("newSchedulerJSONStreamWriter error = %v", err)
			}
		})
	}
}

type markerWriter struct {
	mu     sync.Mutex
	marker string
	data   strings.Builder
	seen   chan struct{}
	once   sync.Once
}

func newMarkerWriter(marker string) *markerWriter {
	return &markerWriter{marker: marker, seen: make(chan struct{})}
}

func (w *markerWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.data.Write(data)
	if strings.Contains(w.data.String(), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

var _ io.Writer = (*markerWriter)(nil)
