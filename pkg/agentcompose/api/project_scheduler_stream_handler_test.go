package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	domain "github.com/chaitin/agent-compose/pkg/model"
	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
	"github.com/chaitin/agent-compose/proto/agentcompose/v2/agentcomposev2connect"
)

func TestStreamSchedulerRunsBatchesAndCompletes(t *testing.T) {
	store, _, handler := newSchedulerRunHandlerFixture()
	startedAt := time.Unix(500, 0).UTC()
	store.runs = []domain.SchedulerRunSummary{
		{ID: "run-3", SchedulerID: store.scheduler.ID, TriggerID: "trigger-1", Status: domain.SchedulerRunStatusSucceeded, StartedAt: startedAt.Add(2 * time.Second)},
		{ID: "run-2", SchedulerID: store.scheduler.ID, TriggerID: "trigger-1", Status: domain.SchedulerRunStatusSucceeded, StartedAt: startedAt.Add(time.Second)},
		{ID: "run-1", SchedulerID: store.scheduler.ID, TriggerID: "trigger-1", Status: domain.SchedulerRunStatusSucceeded, StartedAt: startedAt},
	}
	client, closeServer := schedulerStreamTestClient(t, handler)
	defer closeServer()
	stream, err := client.StreamSchedulerRuns(context.Background(), connect.NewRequest(&agentcomposev2.StreamSchedulerRunsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: store.project.ID}}, BatchSize: 1, Limit: 2,
	}))
	if err != nil {
		t.Fatalf("StreamSchedulerRuns returned error: %v", err)
	}
	var frames []*agentcomposev2.StreamSchedulerRunsResponse
	for stream.Receive() {
		frames = append(frames, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamSchedulerRuns receive error: %v", err)
	}
	if len(frames) != 3 || frames[0].GetRuns()[0].GetRunId() != "run-3" || frames[1].GetRuns()[0].GetRunId() != "run-2" ||
		!frames[2].GetComplete() || !frames[2].GetTruncated() || frames[2].GetEmittedCount() != 2 || frames[2].GetCheckpoint() == "" {
		t.Fatalf("stream frames = %#v", frames)
	}
	assertSchedulerStreamHeaders(t, stream.ResponseHeader())
}

func TestStreamProjectSchedulerEventsTailsInDisplayOrder(t *testing.T) {
	store, _, handler := newSchedulerRunHandlerFixture()
	createdAt := time.Unix(600, 0).UTC()
	store.events = []domain.SchedulerEvent{
		{ID: "event-3", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1", Type: "scheduler.log", CreatedAt: createdAt.Add(2 * time.Second)},
		{ID: "event-2", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1", Type: "scheduler.log", CreatedAt: createdAt.Add(time.Second)},
		{ID: "event-1", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1", Type: "scheduler.log", CreatedAt: createdAt},
	}
	client, closeServer := schedulerStreamTestClient(t, handler)
	defer closeServer()
	stream, err := client.StreamProjectSchedulerEvents(context.Background(), connect.NewRequest(&agentcomposev2.StreamProjectSchedulerEventsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: store.project.ID}}, BatchSize: 1, Tail: 2,
	}))
	if err != nil {
		t.Fatalf("StreamProjectSchedulerEvents returned error: %v", err)
	}
	var eventIDs []string
	var completed *agentcomposev2.StreamProjectSchedulerEventsResponse
	for stream.Receive() {
		frame := stream.Msg()
		for _, event := range frame.GetEvents() {
			eventIDs = append(eventIDs, event.GetId())
		}
		if frame.GetComplete() {
			completed = frame
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamProjectSchedulerEvents receive error: %v", err)
	}
	if len(eventIDs) != 2 || eventIDs[0] != "event-2" || eventIDs[1] != "event-3" || completed == nil || completed.GetEmittedCount() != 2 {
		t.Fatalf("event ids/completion = %#v / %#v", eventIDs, completed)
	}
	assertSchedulerStreamHeaders(t, stream.ResponseHeader())
}

func TestStreamProjectSchedulerEventsResolvesCommandCompletedFromArtifact(t *testing.T) {
	store, _, handler := newSchedulerRunHandlerFixture()
	sandboxDir := t.TempDir()
	writeSchedulerCommandCellArtifact(t, sandboxDir, "cell-1", "streamed full output")
	handler.sandboxDirs = projectHandlerSandboxDirsFake{"sandbox-1": sandboxDir}

	createdAt := time.Unix(800, 0).UTC()
	store.events = []domain.SchedulerEvent{
		{
			ID: "event-1", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1",
			Type: "scheduler.command.completed", Message: "",
			LinkedSandboxID: "sandbox-1", LinkedCellID: "cell-1", CreatedAt: createdAt,
		},
	}
	client, closeServer := schedulerStreamTestClient(t, handler)
	defer closeServer()
	stream, err := client.StreamProjectSchedulerEvents(context.Background(), connect.NewRequest(&agentcomposev2.StreamProjectSchedulerEventsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: store.project.ID}},
	}))
	if err != nil {
		t.Fatalf("StreamProjectSchedulerEvents returned error: %v", err)
	}
	var messages []string
	for stream.Receive() {
		for _, event := range stream.Msg().GetEvents() {
			messages = append(messages, event.GetMessage())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamProjectSchedulerEvents receive error: %v", err)
	}
	if len(messages) != 1 || messages[0] != "streamed full output" {
		t.Fatalf("messages = %#v, want reconstructed artifact content", messages)
	}
}

func TestStreamProjectSchedulerEventsResolvesManyCommandCompletedEventsInOrder(t *testing.T) {
	store, _, handler := newSchedulerRunHandlerFixture()
	sandboxDir := t.TempDir()
	handler.sandboxDirs = projectHandlerSandboxDirsFake{"sandbox-1": sandboxDir}

	const eventCount = 20 // exceeds maxConcurrentEventMessageResolves, exercising the bounded pool
	createdAt := time.Unix(900, 0).UTC()
	store.events = make([]domain.SchedulerEvent, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		cellID := fmt.Sprintf("cell-%d", i)
		writeSchedulerCommandCellArtifact(t, sandboxDir, cellID, fmt.Sprintf("output-%d", i))
		store.events = append(store.events, domain.SchedulerEvent{
			ID: fmt.Sprintf("event-%d", i), SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1",
			Type: "scheduler.command.completed", Message: "",
			LinkedSandboxID: "sandbox-1", LinkedCellID: cellID, CreatedAt: createdAt.Add(time.Duration(i) * time.Second),
		})
	}

	client, closeServer := schedulerStreamTestClient(t, handler)
	defer closeServer()
	stream, err := client.StreamProjectSchedulerEvents(context.Background(), connect.NewRequest(&agentcomposev2.StreamProjectSchedulerEventsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: store.project.ID}},
	}))
	if err != nil {
		t.Fatalf("StreamProjectSchedulerEvents returned error: %v", err)
	}
	var messages []string
	for stream.Receive() {
		for _, event := range stream.Msg().GetEvents() {
			messages = append(messages, event.GetMessage())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamProjectSchedulerEvents receive error: %v", err)
	}
	if len(messages) != eventCount {
		t.Fatalf("messages count = %d, want %d", len(messages), eventCount)
	}
	for i, message := range messages {
		if want := fmt.Sprintf("output-%d", i); message != want {
			t.Fatalf("messages[%d] = %q, want %q (concurrent resolution must preserve display order)", i, message, want)
		}
	}
}

// Regression for #773: an artifact holding invalid UTF-8 used to reach the
// protobuf string field verbatim, so connect failed the entire stream with
// "internal: marshal message: string field contains invalid UTF-8" and the
// caller lost every event in the response, not just the offending one.
func TestStreamProjectSchedulerEventsSanitizesInvalidUTF8Artifact(t *testing.T) {
	store, _, handler := newSchedulerRunHandlerFixture()
	sandboxDir := t.TempDir()
	writeSchedulerCommandCellArtifact(t, sandboxDir, "cell-1", "MARKER-BEFORE\n\xe7\nMARKER-AFTER\n")
	handler.sandboxDirs = projectHandlerSandboxDirsFake{"sandbox-1": sandboxDir}

	createdAt := time.Unix(1200, 0).UTC()
	store.events = []domain.SchedulerEvent{
		{
			ID: "event-1", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1",
			Type: "scheduler.command.completed", Message: "",
			LinkedSandboxID: "sandbox-1", LinkedCellID: "cell-1", CreatedAt: createdAt,
		},
		{
			ID: "event-2", SchedulerID: store.scheduler.ID, RunID: "run-1", TriggerID: "trigger-1",
			Type: "scheduler.run.completed", Message: "run completed", CreatedAt: createdAt.Add(time.Second),
		},
	}
	client, closeServer := schedulerStreamTestClient(t, handler)
	defer closeServer()
	stream, err := client.StreamProjectSchedulerEvents(context.Background(), connect.NewRequest(&agentcomposev2.StreamProjectSchedulerEventsRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: store.project.ID}},
	}))
	if err != nil {
		t.Fatalf("StreamProjectSchedulerEvents returned error: %v", err)
	}
	messages := map[string]string{}
	completed := false
	for stream.Receive() {
		frame := stream.Msg()
		for _, event := range frame.GetEvents() {
			messages[event.GetId()] = event.GetMessage()
		}
		if frame.GetComplete() {
			completed = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamProjectSchedulerEvents receive error (the whole stream must survive one bad byte, #773): %v", err)
	}
	if !completed {
		t.Fatalf("stream ended without a complete frame")
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want both the bad-byte event and the healthy event", messages)
	}
	if want := "MARKER-BEFORE\n\uFFFD\nMARKER-AFTER\n"; messages["event-1"] != want {
		t.Fatalf("event-1 message = %q, want %q", messages["event-1"], want)
	}
	if messages["event-2"] != "run completed" {
		t.Fatalf("event-2 message = %q, want it untouched", messages["event-2"])
	}
	for id, message := range messages {
		if !utf8.ValidString(message) {
			t.Fatalf("message for %s is not valid UTF-8: %q", id, message)
		}
	}
}

func schedulerStreamTestClient(t *testing.T, handler *ProjectHandler) (agentcomposev2connect.ProjectServiceClient, func()) {
	t.Helper()
	mux := http.NewServeMux()
	path, connectHandler := agentcomposev2connect.NewProjectServiceHandler(handler)
	mux.Handle(path, connectHandler)
	server := httptest.NewServer(mux)
	return agentcomposev2connect.NewProjectServiceClient(server.Client(), server.URL), server.Close
}

func assertSchedulerStreamHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	if headers.Get("Cache-Control") != "no-cache, no-transform" || headers.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream headers = %#v", headers)
	}
}
