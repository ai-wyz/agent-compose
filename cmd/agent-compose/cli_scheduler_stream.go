package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"

	"github.com/chaitin/agent-compose/pkg/compose"
	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
)

const schedulerCLIBatchSize = 100

func streamComposeSchedulerRuns(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentRef, triggerRef, status string, limit uint32, writeBatch func([]composeSchedulerRunItem) error) error {
	agentFilter, triggerID, runStatus, statusText, err := resolveComposeSchedulerRunQuery(ctx, clients, normalized, projectID, agentRef, triggerRef, status)
	if err != nil {
		return err
	}
	stream, err := clients.project.StreamSchedulerRuns(ctx, connect.NewRequest(&agentcomposev2.StreamSchedulerRunsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, AgentName: agentFilter, TriggerId: triggerID,
		Status: runStatus, BatchSize: schedulerCLIBatchSize, Limit: limit,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return fallbackComposeSchedulerRuns(ctx, clients, normalized, projectID, agentRef, triggerRef, status, limit, writeBatch)
		}
		return commandExitErrorForConnect(fmt.Errorf("stream scheduler runs: %w", err))
	}
	received := false
	completed := false
	for stream.Receive() {
		received = true
		frame := stream.Msg()
		items := make([]composeSchedulerRunItem, 0, len(frame.GetRuns()))
		for _, run := range frame.GetRuns() {
			if strings.TrimSpace(run.GetTriggerId()) == "" || (agentFilter != "" && run.GetAgentName() != agentFilter) ||
				(triggerID != "" && run.GetTriggerId() != triggerID) || (statusText != "" && schedulerRunStatusText(run.GetStatus()) != statusText) {
				continue
			}
			scheduler, resolveErr := resolveComposeScheduler(normalized, projectID, run.GetAgentName())
			if resolveErr == nil {
				items = append(items, schedulerRuntimeRunItem(scheduler.SchedulerID, run))
			}
		}
		if len(items) > 0 {
			if err := writeBatch(items); err != nil {
				return err
			}
		}
		if frame.GetComplete() {
			completed = true
		}
	}
	if err := stream.Err(); err != nil {
		if !received && connect.CodeOf(err) == connect.CodeUnimplemented {
			return fallbackComposeSchedulerRuns(ctx, clients, normalized, projectID, agentRef, triggerRef, status, limit, writeBatch)
		}
		return commandExitErrorForConnect(fmt.Errorf("stream scheduler runs: %w", err))
	}
	if !completed {
		return fmt.Errorf("stream scheduler runs: daemon closed the stream before completion")
	}
	return nil
}

func fallbackComposeSchedulerRuns(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentRef, triggerRef, status string, limit uint32, writeBatch func([]composeSchedulerRunItem) error) error {
	runs, err := listComposeSchedulerRuns(ctx, clients, normalized, projectID, agentRef, triggerRef, status, limit)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	return writeBatch(runs)
}

func streamProjectSchedulerLogEvents(ctx context.Context, clients cliServiceClients, projectID, agentName, triggerID, runID string, tail int, writeBatch func([]composeSchedulerLogEvent) error) error {
	if tail == 0 {
		return nil
	}
	if tail > math.MaxUint32 {
		return commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler logs --tail must be at most %d", uint64(math.MaxUint32))}
	}
	streamTail := uint32(0)
	if tail > 0 {
		streamTail = uint32(tail)
	}
	stream, err := clients.project.StreamProjectSchedulerEvents(ctx, connect.NewRequest(&agentcomposev2.StreamProjectSchedulerEventsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, AgentName: agentName, TriggerId: triggerID, RunId: runID,
		BatchSize: schedulerCLIBatchSize, Tail: streamTail,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return fallbackProjectSchedulerLogEvents(ctx, clients, projectID, agentName, triggerID, runID, tail, writeBatch)
		}
		return commandExitErrorForConnect(fmt.Errorf("stream scheduler logs: %w", err))
	}
	received := false
	completed := false
	for stream.Receive() {
		received = true
		frame := stream.Msg()
		items := make([]composeSchedulerLogEvent, 0, len(frame.GetEvents()))
		for _, event := range frame.GetEvents() {
			items = append(items, schedulerLogEventFromProto(event))
		}
		if len(items) > 0 {
			if err := writeBatch(items); err != nil {
				return err
			}
		}
		if frame.GetComplete() {
			completed = true
		}
	}
	if err := stream.Err(); err != nil {
		if !received && connect.CodeOf(err) == connect.CodeUnimplemented {
			return fallbackProjectSchedulerLogEvents(ctx, clients, projectID, agentName, triggerID, runID, tail, writeBatch)
		}
		return commandExitErrorForConnect(fmt.Errorf("stream scheduler logs: %w", err))
	}
	if !completed {
		return fmt.Errorf("stream scheduler logs: daemon closed the stream before completion")
	}
	return nil
}

func fallbackProjectSchedulerLogEvents(ctx context.Context, clients cliServiceClients, projectID, agentName, triggerID, runID string, tail int, writeBatch func([]composeSchedulerLogEvent) error) error {
	events, err := listProjectSchedulerLogEvents(ctx, clients.project, projectID, agentName, triggerID, runID, tail)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	return writeBatch(events)
}

func schedulerLogEventFromProto(event *agentcomposev2.SchedulerEvent) composeSchedulerLogEvent {
	return composeSchedulerLogEvent{
		ID: event.GetId(), RunID: event.GetRunId(), AgentName: event.GetAgentName(), SchedulerID: event.GetSchedulerId(), TriggerID: event.GetTriggerId(),
		Type: schedulerDisplayEventType(event.GetType()), Level: event.GetLevel(), Message: event.GetMessage(), PayloadJSON: event.GetPayloadJson(),
		SandboxID: event.GetLinkedSandboxId(), CellID: event.GetLinkedCellId(), AgentThreadID: event.GetLinkedAgentThreadId(), CreatedAt: formatProtoTimestamp(event.GetCreatedAt()),
	}
}

type schedulerRunsTextStreamWriter struct {
	writer  *tabwriter.Writer
	started bool
}

func newSchedulerRunsTextStreamWriter(out io.Writer) *schedulerRunsTextStreamWriter {
	return &schedulerRunsTextStreamWriter{writer: tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)}
}

func (w *schedulerRunsTextStreamWriter) Write(items []composeSchedulerRunItem) error {
	if err := w.start(); err != nil {
		return err
	}
	for _, run := range items {
		sandboxes := "-"
		if len(run.SandboxIDs) > 0 {
			shortIDs := make([]string, 0, len(run.SandboxIDs))
			for _, sandboxID := range run.SandboxIDs {
				shortIDs = append(shortIDs, shortOpaqueID(sandboxID))
			}
			sandboxes = strings.Join(shortIDs, ",")
		}
		if _, err := fmt.Fprintf(w.writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", run.RunShortID, run.AgentName,
			schedulerRunTriggerLabel(run), run.Status, sandboxes, firstNonEmptyString(run.StartedAt, "-"), formatDurationMs(run.DurationMs)); err != nil {
			return err
		}
	}
	return w.writer.Flush()
}

// schedulerRunTriggerLabel renders a run's trigger for text output: the declared
// trigger name followed by the short trigger id when the daemon resolved a name,
// and the trigger id alone otherwise. Script triggers keep publishing their
// trigger id as the name, so they render unchanged.
func schedulerRunTriggerLabel(run composeSchedulerRunItem) string {
	triggerID := strings.TrimSpace(run.TriggerID)
	if triggerID == "" {
		return "-"
	}
	name := strings.TrimSpace(run.TriggerName)
	if name == "" || name == triggerID {
		return triggerID
	}
	return fmt.Sprintf("%s (%s)", name, shortOpaqueID(triggerID))
}

func (w *schedulerRunsTextStreamWriter) Finish() error {
	if err := w.start(); err != nil {
		return err
	}
	return w.writer.Flush()
}

func (w *schedulerRunsTextStreamWriter) start() error {
	if w.started {
		return nil
	}
	w.started = true
	_, err := fmt.Fprintln(w.writer, "RUN ID\tAGENT\tTRIGGER\tSTATUS\tSANDBOXES\tSTARTED\tDURATION")
	return err
}

type schedulerJSONStreamWriter[T any] struct {
	out       io.Writer
	prefix    []byte
	fieldName string
	started   bool
	written   bool
}

func newSchedulerJSONStreamWriter[T any](out io.Writer, prefix any, fieldName string) (*schedulerJSONStreamWriter[T], error) {
	data, err := json.Marshal(prefix)
	if err != nil {
		return nil, err
	}
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
		return nil, fmt.Errorf("scheduler JSON stream prefix must encode a JSON object")
	}
	return &schedulerJSONStreamWriter[T]{out: out, prefix: data, fieldName: fieldName}, nil
}

func (w *schedulerJSONStreamWriter[T]) Write(items []T) error {
	if err := w.start(); err != nil {
		return err
	}
	for _, item := range items {
		data, err := json.MarshalIndent(item, "", "  ")
		if err != nil {
			return err
		}
		if w.written {
			if _, err := io.WriteString(w.out, ","); err != nil {
				return err
			}
		}
		if _, err := w.out.Write(data); err != nil {
			return err
		}
		w.written = true
	}
	return nil
}

func (w *schedulerJSONStreamWriter[T]) Finish() error {
	if err := w.start(); err != nil {
		return err
	}
	_, err := io.WriteString(w.out, "]}\n")
	return err
}

func (w *schedulerJSONStreamWriter[T]) start() error {
	if w.started {
		return nil
	}
	w.started = true
	if _, err := w.out.Write(w.prefix[:len(w.prefix)-1]); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w.out, ",%q: [", w.fieldName)
	return err
}

type schedulerRunsJSONPrefix struct {
	Project composeSchedulerProjectOutput `json:"project"`
}

type schedulerLogsJSONPrefix struct {
	Project composeSchedulerProjectOutput `json:"project"`
	Run     *composeSchedulerRunItem      `json:"run,omitempty"`
}
