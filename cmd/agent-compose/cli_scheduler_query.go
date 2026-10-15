package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chaitin/agent-compose/internal/projects"
	"github.com/chaitin/agent-compose/pkg/agentcompose/api"
	"github.com/chaitin/agent-compose/pkg/compose"
	"github.com/chaitin/agent-compose/pkg/identity"
	domain "github.com/chaitin/agent-compose/pkg/model"
	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
	"github.com/chaitin/agent-compose/proto/agentcompose/v2/agentcomposev2connect"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
)

type composeSchedulerListOptions struct {
	Verbose bool
}

func runComposeSchedulerListCommand(cmd *cobra.Command, cli cliOptions, options composeSchedulerListOptions, args []string) error {
	clients, err := newCLIServiceClients(cli)
	if err != nil {
		return err
	}
	runtimeProject, err := resolveComposeRuntimeProject(cmd.Context(), clients.project, cli, "scheduler ls")
	if err != nil {
		return err
	}
	normalized := runtimeProject.spec
	projectID := runtimeProject.id()
	agentFilter := ""
	if len(args) > 0 {
		agentFilter, err = resolveComposeAgentNameFromSpec(normalized, projectID, args[0])
		if err != nil {
			return err
		}
	}
	triggers, err := listComposeSchedulerTriggers(cmd.Context(), clients, normalized, projectID, agentFilter)
	if err != nil {
		return err
	}
	output := composeSchedulerListOutput{Triggers: triggers}
	if cli.JSON {
		output.Project = schedulerProjectOutput(cmd.Context(), clients.project, runtimeProject)
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		return writeCommandOutput(cmd.OutOrStdout(), append(data, '\n'))
	}
	return writeSchedulerListText(cmd.OutOrStdout(), output, options.Verbose)
}

func shouldResolveSchedulerRunRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	return shouldResolveComposeLogResourceRef(ref) || (len(ref) >= 6 && strings.Contains(ref, "-"))
}

func listComposeSchedulerTriggers(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentFilter string) ([]composeSchedulerTriggerItem, error) {
	var items []composeSchedulerTriggerItem
	for _, agent := range normalized.Agents {
		if agentFilter != "" && agent.Name != agentFilter {
			continue
		}
		if agent.Scheduler == nil {
			continue
		}
		schedulerID, err := projects.StableProjectSchedulerID(projectID, agent.Name, "")
		if err != nil {
			return nil, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("resolve scheduler for agent %q: %w", agent.Name, err)}
		}
		schedulerEnabled := agent.Scheduler.Enabled
		if agent.Scheduler.HasScript() {
			scheduler, err := clients.project.GetScheduler(ctx, connect.NewRequest(&agentcomposev2.GetSchedulerRequest{Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, AgentName: agent.Name}))
			if err != nil {
				return nil, commandExitErrorForConnect(fmt.Errorf("get scheduler %s: %w", schedulerID, err))
			}
			for _, trigger := range scheduler.Msg.GetTriggers() {
				items = append(items, schedulerTriggerItemFromResolved(agent.Name, schedulerID, schedulerEnabled, trigger))
			}
			continue
		}
		for index, trigger := range agent.Scheduler.Triggers {
			id, err := projects.StableSchedulerTriggerID(projectID, agent.Name, "", trigger.Name, index)
			if err != nil {
				return nil, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("resolve trigger for agent %q: %w", agent.Name, err)}
			}
			items = append(items, schedulerTriggerItemFromDeclarative(agent.Name, schedulerID, schedulerEnabled, id, trigger))
		}
	}
	if agentFilter != "" && len(items) == 0 {
		if _, ok := composeRunAgentSpec(normalized, agentFilter); !ok {
			return nil, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("agent %q is not configured in this project", agentFilter)}
		}
	}
	return items, nil
}

func listComposeSchedulerRuns(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentRef, triggerRef, status string, limit uint32) ([]composeSchedulerRunItem, error) {
	agentFilter, triggerID, runStatus, statusText, err := resolveComposeSchedulerRunQuery(ctx, clients, normalized, projectID, agentRef, triggerRef, status)
	if err != nil {
		return nil, err
	}
	items := make([]composeSchedulerRunItem, 0)
	var offset uint32
	for {
		pageLimit := uint32(500)
		if limit > 0 && limit-uint32(len(items)) < pageLimit {
			pageLimit = limit - uint32(len(items))
		}
		if pageLimit == 0 {
			return items, nil
		}
		resp, err := clients.project.ListSchedulerRuns(ctx, connect.NewRequest(&agentcomposev2.ListSchedulerRunsRequest{
			Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, AgentName: agentFilter, TriggerId: triggerID, Status: runStatus, Limit: pageLimit, Offset: offset,
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				return nil, commandExitError{Code: exitCodeUnsupported, Err: fmt.Errorf("daemon does not support complete scheduler run queries; upgrade the daemon")}
			}
			return nil, commandExitErrorForConnect(fmt.Errorf("list scheduler runs: %w", err))
		}
		runs := resp.Msg.GetRuns()
		for _, run := range runs {
			if strings.TrimSpace(run.GetTriggerId()) == "" || (agentFilter != "" && run.GetAgentName() != agentFilter) ||
				(triggerID != "" && run.GetTriggerId() != triggerID) || (statusText != "" && schedulerRunStatusText(run.GetStatus()) != statusText) {
				continue
			}
			scheduler, resolveErr := resolveComposeScheduler(normalized, projectID, run.GetAgentName())
			if resolveErr != nil {
				continue
			}
			items = append(items, schedulerRuntimeRunItem(scheduler.SchedulerID, run))
			if limit > 0 && uint32(len(items)) >= limit {
				return items, nil
			}
		}
		offset += uint32(len(runs))
		if offset >= resp.Msg.GetTotal() {
			return items, nil
		}
		if len(runs) == 0 {
			return nil, fmt.Errorf("scheduler run pagination did not advance")
		}
	}
}

func resolveComposeSchedulerRunQuery(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentRef, triggerRef, status string) (string, string, agentcomposev2.SchedulerRunStatus, string, error) {
	agentFilter := ""
	if strings.TrimSpace(agentRef) != "" {
		scheduler, resolveErr := resolveComposeScheduler(normalized, projectID, agentRef)
		if resolveErr != nil {
			return "", "", agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED, "", resolveErr
		}
		agentFilter = scheduler.AgentName
	}
	runStatus, statusText, statusErr := parseSchedulerRunStatusFilter(status)
	if statusErr != nil {
		return "", "", agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED, "", statusErr
	}
	triggerRef = strings.TrimSpace(triggerRef)
	triggerID := ""
	if triggerRef != "" {
		resolvedTriggerID, resolveErr := resolveSchedulerTriggerIDForQuery(ctx, clients, normalized, projectID, agentFilter, triggerRef)
		if resolveErr != nil {
			if errors.Is(resolveErr, domain.ErrAmbiguous) && agentFilter == "" {
				resolveErr = fmt.Errorf("%w; specify a scheduler argument to disambiguate", resolveErr)
			}
			if isSchedulerResourceNotFound(resolveErr) || errors.Is(resolveErr, domain.ErrAmbiguous) {
				return "", "", agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED, "", commandExitError{Code: exitCodeUsage, Err: resolveErr}
			}
			return "", "", agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED, "", resolveErr
		}
		triggerID = resolvedTriggerID
	}
	return agentFilter, triggerID, runStatus, statusText, nil
}

func resolveSchedulerTriggerIDForQuery(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentFilter, triggerRef string) (string, error) {
	triggers, err := listComposeSchedulerTriggers(ctx, clients, normalized, projectID, agentFilter)
	if err != nil {
		return "", err
	}
	trigger, err := resolveSchedulerTriggerFromItems(triggers, triggerRef)
	if err == nil {
		return firstNonEmptyString(trigger.RawTriggerID, trigger.TriggerID), nil
	}
	if !isSchedulerResourceNotFound(err) {
		return "", err
	}

	historicalID, found, historicalErr := resolveHistoricalSchedulerTriggerID(ctx, clients.project, normalized, projectID, agentFilter, triggerRef)
	if historicalErr != nil {
		return "", historicalErr
	}
	if !found {
		return "", err
	}
	return historicalID, nil
}

func resolveHistoricalSchedulerTriggerID(ctx context.Context, client agentcomposev2connect.ProjectServiceClient, normalized *compose.NormalizedProjectSpec, projectID, agentFilter, rawRef string) (string, bool, error) {
	triggerID := strings.TrimSpace(rawRef)
	if hash, err := identity.Hash(triggerID); err == nil {
		triggerID = hash
	}

	matchedAgents := make([]string, 0, 1)
	for _, agent := range normalized.Agents {
		if agent.Scheduler == nil || (agentFilter != "" && agent.Name != agentFilter) {
			continue
		}
		resp, err := client.ListSchedulerRuns(ctx, connect.NewRequest(&agentcomposev2.ListSchedulerRunsRequest{
			Project:   &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}},
			AgentName: agent.Name,
			TriggerId: triggerID,
			Limit:     1,
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				return "", false, commandExitError{Code: exitCodeUnsupported, Err: fmt.Errorf("daemon does not support complete scheduler run queries; upgrade the daemon")}
			}
			return "", false, commandExitErrorForConnect(fmt.Errorf("probe scheduler run history for trigger %s: %w", triggerID, err))
		}
		for _, run := range resp.Msg.GetRuns() {
			if run.GetAgentName() == agent.Name && run.GetTriggerId() == triggerID {
				matchedAgents = append(matchedAgents, agent.Name)
				break
			}
		}
	}
	if len(matchedAgents) == 0 {
		return "", false, nil
	}
	if len(matchedAgents) > 1 {
		return "", false, domain.ResourceError(domain.ErrAmbiguous, "historical scheduler trigger", rawRef, fmt.Sprintf("historical scheduler trigger ID %q is ambiguous", rawRef), nil)
	}
	return triggerID, true, nil
}

func parseSchedulerRunStatusFilter(value string) (agentcomposev2.SchedulerRunStatus, string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	statuses := map[string]agentcomposev2.SchedulerRunStatus{
		"":          agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED,
		"running":   agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_RUNNING,
		"succeeded": agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_SUCCEEDED,
		"failed":    agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_FAILED,
		"canceled":  agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_CANCELED,
		"skipped":   agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_SKIPPED,
	}
	status, ok := statuses[value]
	if !ok {
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED, "", commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler runs --status must be running, succeeded, failed, canceled, or skipped")}
	}
	return status, value, nil
}

func schedulerRuntimeRunItem(schedulerID string, run *agentcomposev2.SchedulerRun) composeSchedulerRunItem {
	return composeSchedulerRunItem{
		RunID:         run.GetRunId(),
		RunShortID:    shortOpaqueID(run.GetRunId()),
		AgentName:     run.GetAgentName(),
		SchedulerID:   firstNonEmptyString(run.GetSchedulerId(), schedulerID),
		TriggerID:     run.GetTriggerId(),
		TriggerName:   run.GetTriggerName(),
		TriggerKind:   triggerKindText(run.GetTriggerKind()),
		TriggerSource: run.GetTriggerSource(),
		Status:        schedulerRunStatusText(run.GetStatus()),
		SandboxIDs:    append([]string(nil), run.GetSandboxIds()...),
		StartedAt:     formatProtoTimestamp(run.GetStartedAt()),
		CompletedAt:   formatProtoTimestamp(run.GetCompletedAt()),
		DurationMs:    run.GetDurationMs(),
		Error:         run.GetError(),
		ResultJSON:    run.GetResultJson(),
		PayloadJSON:   run.GetPayloadJson(),
		ArtifactsDir:  run.GetArtifactsDir(),
	}
}

func resolveSchedulerRuntimeRun(ctx context.Context, client agentcomposev2connect.ProjectServiceClient, normalized *compose.NormalizedProjectSpec, projectID, ref string) (*composeSchedulerRunItem, error) {
	ref = strings.TrimSpace(ref)
	response, err := client.GetSchedulerRun(ctx, connect.NewRequest(&agentcomposev2.GetSchedulerRunRequest{
		Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, RunId: ref,
	}))
	if err == nil && response != nil && response.Msg.GetRun() != nil {
		run := response.Msg.GetRun()
		schedulerID, idErr := projects.StableProjectSchedulerID(projectID, run.GetAgentName(), "")
		if idErr != nil {
			return nil, idErr
		}
		item := schedulerRuntimeRunItem(firstNonEmptyString(run.GetSchedulerId(), schedulerID), run)
		return &item, nil
	}
	if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return nil, commandExitErrorForConnect(fmt.Errorf("get scheduler run %s: %w", ref, err))
	}
	_ = normalized
	return nil, schedulerResourceNotFoundError{kind: "run", ref: ref}
}

func listProjectSchedulerLogEvents(ctx context.Context, client agentcomposev2connect.ProjectServiceClient, projectID, agentName, triggerID, runID string, tail int) ([]composeSchedulerLogEvent, error) {
	events := make([]composeSchedulerLogEvent, 0)
	if tail == 0 {
		return events, nil
	}
	var offset uint32
	for {
		pageLimit := uint32(500)
		if tail > 0 && tail-len(events) < int(pageLimit) {
			pageLimit = uint32(tail - len(events))
		}
		if pageLimit == 0 {
			break
		}
		resp, err := client.ListProjectSchedulerEvents(ctx, connect.NewRequest(&agentcomposev2.ListProjectSchedulerEventsRequest{
			Project: &agentcomposev2.ProjectRef{Selector: &agentcomposev2.ProjectRef_ProjectId{ProjectId: projectID}}, AgentName: agentName, TriggerId: triggerID, RunId: runID, Limit: pageLimit, Offset: offset,
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				return nil, commandExitError{Code: exitCodeUnsupported, Err: fmt.Errorf("daemon does not support complete scheduler log queries; upgrade the daemon")}
			}
			return nil, commandExitErrorForConnect(fmt.Errorf("list scheduler logs: %w", err))
		}
		pageEvents := resp.Msg.GetEvents()
		for _, event := range pageEvents {
			events = append(events, composeSchedulerLogEvent{
				ID: event.GetId(), RunID: event.GetRunId(), AgentName: event.GetAgentName(), SchedulerID: event.GetSchedulerId(), TriggerID: event.GetTriggerId(),
				Type: schedulerDisplayEventType(event.GetType()), Level: event.GetLevel(), Message: event.GetMessage(), PayloadJSON: event.GetPayloadJson(),
				SandboxID: event.GetLinkedSandboxId(), CellID: event.GetLinkedCellId(), AgentThreadID: event.GetLinkedAgentThreadId(), CreatedAt: formatProtoTimestamp(event.GetCreatedAt()),
			})
			if tail > 0 && len(events) >= tail {
				break
			}
		}
		if tail > 0 && len(events) >= tail {
			break
		}
		offset += uint32(len(pageEvents))
		if offset >= resp.Msg.GetTotal() {
			break
		}
		if len(pageEvents) == 0 {
			return nil, fmt.Errorf("scheduler event pagination did not advance")
		}
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

func resolveComposeScheduler(normalized *compose.NormalizedProjectSpec, projectID, ref string) (*composeSchedulerItem, error) {
	ref = strings.TrimSpace(ref)
	matches := make([]composeSchedulerItem, 0)
	for _, agent := range normalized.Agents {
		if agent.Scheduler == nil {
			continue
		}
		schedulerID, err := projects.StableProjectSchedulerID(projectID, agent.Name, "")
		if err != nil {
			return nil, err
		}
		if resourceRefMatches(ref, agent.Name, schedulerID) {
			matches = append(matches, composeSchedulerItem{AgentName: agent.Name, SchedulerID: schedulerID, Enabled: agent.Scheduler.Enabled, TriggerCount: len(agent.Scheduler.Triggers)})
		}
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		return nil, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler reference %q is ambiguous", ref)}
	}
	return nil, schedulerResourceNotFoundError{kind: "resource", ref: ref}
}

func resolveSchedulerTriggerFromItems(items []composeSchedulerTriggerItem, ref string) (*composeSchedulerTriggerItem, error) {
	matches := make([]composeSchedulerTriggerItem, 0)
	for _, item := range items {
		if resourceRefMatches(ref, item.Name, item.TriggerID, item.RawTriggerID) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		return nil, domain.ResourceError(domain.ErrAmbiguous, "scheduler trigger", ref, fmt.Sprintf("scheduler trigger reference %q is ambiguous", ref), nil)
	}
	return nil, schedulerResourceNotFoundError{kind: "trigger", ref: ref}
}

func resolveComposeSchedulerTrigger(ctx context.Context, clients cliServiceClients, normalized *compose.NormalizedProjectSpec, projectID, agentName, triggerRef string) (composeSchedulerTriggerItem, error) {
	triggerRef = strings.TrimSpace(triggerRef)
	if strings.TrimSpace(agentName) == "" || triggerRef == "" {
		return composeSchedulerTriggerItem{}, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler trigger requires non-empty agent and trigger")}
	}
	scheduler, err := resolveComposeScheduler(normalized, projectID, agentName)
	if err != nil {
		return composeSchedulerTriggerItem{}, err
	}
	agentName = scheduler.AgentName
	items, err := listComposeSchedulerTriggers(ctx, clients, normalized, projectID, agentName)
	if err != nil {
		return composeSchedulerTriggerItem{}, err
	}
	trigger, err := resolveSchedulerTriggerFromItems(items, triggerRef)
	if isSchedulerResourceNotFound(err) {
		return composeSchedulerTriggerItem{}, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler trigger %q not found for agent %q", triggerRef, agentName)}
	}
	if err != nil {
		return composeSchedulerTriggerItem{}, commandExitError{Code: exitCodeUsage, Err: fmt.Errorf("scheduler trigger %q for agent %q is ambiguous; use the trigger id", triggerRef, agentName)}
	}
	return *trigger, nil
}

func schedulerTriggerItemFromDeclarative(agentName, schedulerID string, schedulerEnabled bool, triggerID string, trigger compose.NormalizedTriggerSpec) composeSchedulerTriggerItem {
	protoTrigger := api.TriggerSpecToProto(trigger)
	return composeSchedulerTriggerItem{
		AgentName:        agentName,
		Name:             strings.TrimSpace(trigger.Name),
		TriggerID:        displayOpaqueID(triggerID),
		TriggerShortID:   shortOpaqueID(triggerID),
		RawTriggerID:     triggerID,
		Kind:             trigger.Kind,
		Source:           "declarative",
		SchedulerID:      displayOpaqueID(schedulerID),
		SchedulerShortID: shortOpaqueID(schedulerID),
		RawSchedulerID:   schedulerID,
		SchedulerEnabled: schedulerEnabled,
		TriggerEnabled:   true,
		declarative:      protoTrigger,
	}
}

func schedulerTriggerItemFromResolved(agentName, schedulerID string, schedulerEnabled bool, trigger *agentcomposev2.ResolvedTrigger) composeSchedulerTriggerItem {
	interval, _ := time.ParseDuration(trigger.GetSpec().GetInterval())
	kind := triggerKindText(trigger.GetSpec().GetKind())
	registered := map[string]any{"scheduler_id": schedulerID, "trigger_id": trigger.GetTriggerId(), "kind": kind, "enabled": trigger.GetEnabled(), "auto_id": false, "interval_ms": interval.Milliseconds(), "topic": trigger.GetSpec().GetEvent().GetTopic(), "spec_json": "", "next_fire_at": formatProtoTimestamp(trigger.GetNextFireAt()), "last_fired_at": formatProtoTimestamp(trigger.GetLastFiredAt())}
	return composeSchedulerTriggerItem{
		AgentName:        agentName,
		TriggerID:        displayOpaqueID(trigger.GetTriggerId()),
		TriggerShortID:   shortOpaqueID(trigger.GetTriggerId()),
		RawTriggerID:     trigger.GetTriggerId(),
		Kind:             kind,
		Source:           "script",
		SchedulerID:      displayOpaqueID(schedulerID),
		SchedulerShortID: shortOpaqueID(schedulerID),
		RawSchedulerID:   schedulerID,
		SchedulerEnabled: schedulerEnabled,
		TriggerEnabled:   trigger.GetEnabled(), Topic: trigger.GetSpec().GetEvent().GetTopic(), IntervalMs: interval.Milliseconds(), NextFireAt: formatProtoTimestamp(trigger.GetNextFireAt()), LastFiredAt: formatProtoTimestamp(trigger.GetLastFiredAt()), registered: registered,
	}
}

type composeSchedulerItem struct {
	AgentName    string `json:"agent_name"`
	SchedulerID  string `json:"scheduler_id"`
	Enabled      bool   `json:"enabled"`
	TriggerCount int    `json:"trigger_count"`
}

type composeSchedulerRunItem struct {
	RunID         string   `json:"run_id"`
	RunShortID    string   `json:"run_short_id"`
	AgentName     string   `json:"agent_name"`
	SchedulerID   string   `json:"scheduler_id"`
	TriggerID     string   `json:"trigger_id,omitempty"`
	TriggerName   string   `json:"trigger_name,omitempty"`
	TriggerKind   string   `json:"trigger_kind,omitempty"`
	TriggerSource string   `json:"trigger_source,omitempty"`
	Status        string   `json:"status"`
	SandboxIDs    []string `json:"sandbox_ids,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	CompletedAt   string   `json:"completed_at,omitempty"`
	DurationMs    int64    `json:"duration_ms,omitempty"`
	Error         string   `json:"error,omitempty"`
	ResultJSON    string   `json:"result_json,omitempty"`
	PayloadJSON   string   `json:"payload_json,omitempty"`
	ArtifactsDir  string   `json:"artifacts_dir,omitempty"`
}

type composeSchedulerLogEvent struct {
	ID            string `json:"id"`
	RunID         string `json:"run_id"`
	AgentName     string `json:"agent_name"`
	SchedulerID   string `json:"scheduler_id"`
	TriggerID     string `json:"trigger_id,omitempty"`
	Type          string `json:"type"`
	Level         string `json:"level"`
	Message       string `json:"message,omitempty"`
	PayloadJSON   string `json:"payload_json,omitempty"`
	SandboxID     string `json:"sandbox_id,omitempty"`
	CellID        string `json:"cell_id,omitempty"`
	AgentThreadID string `json:"agent_thread_id,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

type composeSchedulerTriggerItem struct {
	AgentName        string `json:"agent_name"`
	Name             string `json:"name,omitempty"`
	TriggerID        string `json:"trigger_id"`
	TriggerShortID   string `json:"trigger_short_id"`
	RawTriggerID     string `json:"-"`
	Kind             string `json:"kind"`
	Source           string `json:"source"`
	SchedulerID      string `json:"scheduler_id,omitempty"`
	SchedulerShortID string `json:"scheduler_short_id,omitempty"`
	RawSchedulerID   string `json:"-"`
	SchedulerEnabled bool   `json:"scheduler_enabled"`
	TriggerEnabled   bool   `json:"trigger_enabled"`
	Topic            string `json:"topic,omitempty"`
	IntervalMs       int64  `json:"interval_ms,omitempty"`
	SpecJSON         string `json:"spec_json,omitempty"`
	NextFireAt       string `json:"next_fire_at,omitempty"`
	LastFiredAt      string `json:"last_fired_at,omitempty"`
	declarative      *agentcomposev2.TriggerSpec
	registered       map[string]any
}
