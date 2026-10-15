package api

import (
	"strings"

	domain "github.com/chaitin/agent-compose/pkg/model"
	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
)

func schedulerRunToProto(run domain.SchedulerRunSummary, scheduler domain.ProjectSchedulerRecord) *agentcomposev2.SchedulerRun {
	return &agentcomposev2.SchedulerRun{
		RunId:              run.ID,
		ProjectId:          scheduler.ProjectID,
		AgentName:          scheduler.AgentName,
		SchedulerId:        scheduler.SchedulerID,
		TriggerId:          run.TriggerID,
		TriggerName:        declaredTriggerName(scheduler, run.TriggerID),
		TriggerKind:        triggerKindToProto(run.TriggerKind),
		TriggerSource:      run.TriggerSource,
		Status:             schedulerRunStatusToProto(run.Status),
		StartedAt:          projectTimestamp(run.StartedAt),
		CompletedAt:        projectTimestamp(run.CompletedAt),
		DurationMs:         run.DurationMs,
		Error:              run.Error,
		ResultJson:         run.ResultJSON,
		PayloadJson:        run.PayloadJSON,
		SourceScriptSha256: run.SourceScriptHash,
		ArtifactsDir:       run.ArtifactsDir,
	}
}

// declaredTriggerName resolves the declared name of the trigger that produced a
// run, so clients can show a readable trigger instead of the stable trigger id.
// Script triggers, and runs whose trigger is no longer declared, resolve to "".
func declaredTriggerName(scheduler domain.ProjectSchedulerRecord, triggerID string) string {
	trigger := declaredTriggerSpec(scheduler, triggerID)
	if trigger == nil {
		return ""
	}
	return strings.TrimSpace(trigger.GetName())
}

func schedulerRunStatusToProto(status string) agentcomposev2.SchedulerRunStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case domain.SchedulerRunStatusRunning:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_RUNNING
	case domain.SchedulerRunStatusSucceeded:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_SUCCEEDED
	case domain.SchedulerRunStatusFailed:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_FAILED
	case domain.SchedulerRunStatusCanceled:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_CANCELED
	case domain.SchedulerRunStatusSkipped:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_SKIPPED
	default:
		return agentcomposev2.SchedulerRunStatus_SCHEDULER_RUN_STATUS_UNSPECIFIED
	}
}
