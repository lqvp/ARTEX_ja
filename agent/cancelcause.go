package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "Task paused by the user",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was deliberately cancelled; the in-flight intent returns to frontier(open) and is re-claimed and re-run from scratch after the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Task paused by the orchestrator agent",
		"The orchestrator agent called the pause_task tool to pause this task. This Planner/Worker run was deliberately cancelled; the in-flight intent returns to frontier(open) and is re-run after resume")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the delete barrier has cancelled its running Planner, Worker, and main agent; the result of this run will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the paused state",
		"On startup the backend restored the task pause from the state persisted in the database. This run was cancelled; normally no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner decided the goal was met",
		"The planner decided the task goal was met and set the task to done, then cancelled the still-running Workers; those intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout settlement ran out of time",
		"After the task hit timeout it waited for running Workers to settle gracefully, but the 90-second drain grace was still not enough, so a hard cancel was executed; intents are marked exhausted, and facts and assets already written during settlement are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner called kill_work to deliberately terminate this intent, usually because the direction drifted or there is no value in continuing; the intent is marked stopped and will not be re-claimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused the running Worker. This call was cancelled and the intent turns to paused; all previously registered intents, facts, findings, and activity records are kept, and it re-runs from scratch after resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted the running Worker. This call was cancelled; after the Worker leaves the write zone, the server handles the intent according to the deletion mode the user chose — fake delete only marks it deleted and keeps all output, real delete cascades away the intent and the downstream nodes it alone supports")
	AbortWorkFinished = cause("work_finished", "Worker finished and released context",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, the cancellation and the settle event raced")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run refused while paused",
		"While the task is paused the engine refuses to hand out a new execution context, preventing a race between claim and pause from letting the Worker keep starting; claimed intents return to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this turn",
		"The user clicked stop, deliberately aborting this main-agent or conversation-agent run. Activity records already produced are kept, and the next message can be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause aborted the main-agent chat",
		"When the user paused the task, the running main-agent chat was cancelled at the same time. Activity records already produced are kept; the turn is not replayed automatically after resume")
	AbortChatTurnFinished = cause("chat_turn_finished", "Turn finished and released context",
		"This turn finished normally and the server is releasing that turn's context resources. This is not a run interruption; if it appears in an interruption message, the cancellation and the settle event raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, upgrading, or shutting down. All running agents are cancelled; leftover running intents are reset to open after restart and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "Hard timeout backstop for a run fired",
		"A run exceeded the soft wall-clock budget plus extra grace, meaning a model request or a tool did not return for a long time, so normal turn-boundary settlement could not run. Focus on the last tool call that had not returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached its deadline",
			"Upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller gave no named cause",
			"Upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; register the cause in agent/cancelcause.go and wire it into that cancellation site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
