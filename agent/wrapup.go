package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up/settlement prompt: injected when an agent is terminated due to [MaxTurns exhausted] or
// [timeout (run_seconds/MaxDuration)]; the SDK settlement stage injects this prompt,
// asking the agent to persist recognized-but-unwritten results, then give a one-line summary to avoid half-finished output.
//
// Per-agent wrap-up prompt can be overridden in admin UI (stored in agents.wrapup_prompt); empty → built-in default here.
// Only the prompt body is editable; disabled tools and wrap-up turn budget are code-fixed policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. worker reuses the historically hardcoded
// settleWrapUpPrompt (defined in worker.go); planner/mainagent each have their own; unmatched (custom agents) use the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in per-agent turn budget for the wrap-up phase itself (overridable
// by admin >0). All 10 turns, giving enough steps to persist. Unmatched → genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are nearly exhausted — note that only [this round] ends; the system will wake you again later to continue planning as the situation evolves. This is not a task termination, and you do not need to wrap up the entire planning here. Land the conclusions you have already worked out this round so it is not wasted, but [do not force intents out just to close the round] (zero intents this round is still a perfectly normal result): (1) if you have determined an exploration direction that [should be dispatched now], submit it in one batch with add_intent (do not sit on ready ideas); (2) for goals already proven achieved by a finding/fact, call prove_goal to mark them met (do not miss any); (3) if you identify a serial exploitation chain that needs to be staged, note it down with TodoWrite so the next wake-up can continue dispatching. When done, end this round directly — no summary text is needed."

const mainAgentWrapUpDefault = "Your steps are nearly exhausted and this interaction is about to end. Do not start any new exploration/operations. Please summarize the current progress, key conclusions, and suggested next step for the user **in a single plain-text sentence**."

const genericWrapUpDefault = "You are about to be terminated due to budget exhaustion. First write back the completed but not-yet-persisted results, then **in a single plain-text sentence** summarize what you did and what key conclusions you reached (this sentence will be shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up wording (see docs/task-timeout-and-wrapup-design.md) ----------
//
// Two separate aspects vs the per-run wrap-up: per-run means "this run's budget is spent"; task timeout means
// "the whole task hit its deadline and is about to end". The semantics often oppose (esp. planner: per-run says
// "don't stop, keep planning"; task timeout says "stop planning, make the final call"). Configured only for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB override for task-timeout wrap-up wording and turn count
// (wired to agents.task_timeout_wrapup_prompt / _max_turns; worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this run's budget — the whole exploration is out of time). This is the last chance: (1) write back [ALL] the content you identified but have not yet persisted — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) do not start any new commands/probes; (3) **finally, in a single plain-text sentence**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not this round — the whole task is terminating). Please make one final goal judgment based on the [current] complete set of facts and findings: call prove_goal to mark met any goal already proven achieved by evidence (do not miss any). **Do not generate any new intents** (intents dispatched at this point will not be executed anymore). Wrap up right after the judgment — no summary text is needed."

// TaskTimeoutWrapupDefault returns the built-in task-timeout default wrap-up wording for an agent (admin placeholder/restore-default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // not configured (mainagent/chat) → empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. Empty string means that agent has no task-timeout wording
// (not worker/planner); caller should fall back to the per-run wording.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // default: inherit per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: Timeout wrap-up = task deadline reached → task-timeout wording;
//     MaxTurns wrap-up = steps exhausted inside the clamp window, task still has minutes → fall back to per-run wording.
//   - clamped=false → task still early: both reasons use the per-run wording (degrades to wrapupSettlement).
//
// Delegated to harness's PromptByReason, which picks at wrap-up time based on the [actual] reason, avoiding build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task deadline reached
				harness.ReasonMaxTurns: perRun, // turns exhausted first, task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
