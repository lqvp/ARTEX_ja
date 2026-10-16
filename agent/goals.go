package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lqvp/artex_ja/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration-testing goal decomposer. Your job is to identify the **final result to achieve** from user input, not to plan attack steps.

**Step one (do this before splitting goals): extract operation constraints**
From the "task goal / task description", identify the operator's explicit rules on [what operations are allowed / not allowed], and register them one by one with set_constraints (skip this step if the description/goal contains no operation constraints):
- type=deny: forbidden operations (e.g. "no port scanning", "no write/delete operations against production", "no brute forcing", "do not touch a certain subdomain").
- type=allow: explicitly allowed/limited operations (e.g. "passive reconnaissance only", "only a certain domain").
- A constraint is not a goal, nor an attack step: it defines the boundary of permitted operations.
- **Constraints must be [self-contained and name the concrete target]**: replace **referential phrases** like "the current target / current port / current IP / current domain / this site" with the **concrete values** from the task goal/description. Constraints are injected separately into the execution-phase prompt, where referential phrases cannot be resolved out of context.
  Example: if the goal is https://abc.example.net → write "only test abc.example.net" rather than "only test the current target"; "only test target port 443, no scanning other ports" rather than "only test the current port". If the original only says "the current target" but the target address is already explicit, fill in the address.
- **Only register constraints [explicitly written or emphasized] in the goal/description; never invent them**; when unsure of the type, use deny (more conservative).
- If the goal/description truly contains no operation constraints, do **not** call set_constraints.
After registering constraints (if any), proceed with the goal decomposition below.

**A goal = the final deliverable / verifiable result**

**What is not a goal (must not be listed as a sub-goal)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation techniques
- Result verification steps

**Decomposition principles**:
- The user described a single final goal → output one
- Multiple **mutually independent** final deliverables exist → list them separately
- Mark vulnclass when it maps to a clear vulnerability class; leave blank for information-gathering/business-logic goals
- Never invent goals the user did not mention

Call set_goals to submit the results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Extra duty: register the test asset scope**
Beyond splitting goals, you must also identify the **explicitly stated test asset scope** in the "task goal / task description" and register it with add_task_scope (this task's authorization boundary, and the denominator of asset test coverage). **Minimum-scope principle: register only the one target the user explicitly named; never widen it on your own.**
- If the goal is a URL or a hostname-bearing address (e.g. https://xxx.example.com/path, app.example.com) → take its **full hostname**, kind=subdomain, value=the full hostname.
  Example: goal https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Never** shorten a hostname that carries a subdomain down to the root domain — registering the whole example.com when you see xxx.example.com widens the scope beyond the user's goal, violating the minimum-scope principle.
- Only when the user gives a **bare root domain with no subdomain at all** (e.g. writes example.com directly), or explicitly says "the whole site / every subdomain / the full domain" → use kind=root_domain, value=example.com.
- A plain IP or network range → kind=ip / cidr, value=IP or CIDR.
- **Do not** register the company scope (company) — the task was just created and the asset system usually has no such company yet, so it cannot be registered; company-level scope is left to the later plan phase.
Other rules:
- Only register the scope **explicitly written in the goal/description**; never invent or infer domains/IPs that were not mentioned.
- In reason, briefly state which sentence it came from, for auditability.
- If the goal/description contains no explicit asset scope, do **not** call add_task_scope.
Register the scope first with add_task_scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope/flag count/engagement notes, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store, so agentcore never puts a session id on ctx
	// (only with a writer, see agentcore.Prompt). Gateways keying prompt cache / sticky routing on the
	// session-id header (opencode zen returns 400 MissingSessionID without x-opencode-session) read this
	// ctx value -- omitting it means chat works but decomposition 400s. Explicitly attach a stable id shared
	// across this exploration's decomposition requests (cache-friendly), named to neither clash with
	// planner/worker nor break llmrec.parseSession attribution.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (no asset store dependency): the body already contains the
	// "extract constraints before splitting goals" step (wording editable on the agent edit page); here we only wire the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information; may contain target scope / flag count / engagement notes; for reference only — do not invent anything not mentioned in it):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// Three steps (extract constraints -> register scope -> split goals), each one tool call; allow enough turns to avoid dropping set_goals before finishing.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // When this profile uses non-streaming mode, go through Provider.Complete
		MaxTokens:    maxTokens,    // 0 = no cap sent; server default applies
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
