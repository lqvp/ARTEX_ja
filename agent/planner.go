package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/lqvp/artex_ja/db"
	"github.com/lqvp/artex_ja/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todos (kept across wake-ups, written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Act on them: dispatch an intent only for a next step whose [prerequisites are done / whose dependent facts already exist]; update the list with TodoWrite (mark fact-satisfied steps completed). Don't re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = goal text newly added this time, 1+ items; set_goals supports batches).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: intent summary captured before delete (node gone; cannot re-query)
	Goals    []string // Kind=="goal" only: goal text(s) added by this set_goals call (1 or more)
	OldGoal  string   // Kind=="goal_edited" only: goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: goal text after the edit
	Hints    []string // Kind=="hint" only: hint text(s) added by this add_hint call (1 or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Actual changes that triggered this round (look here first before deciding whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one goal: %s — a new goal to attain; add exploration directions accordingly (if no matching intent exists yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all new goals to attain; for each one without a matching intent, add exploration directions accordingly.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one strategic hint: %s — it is attached to the exploration graph; adjust/add exploration directions accordingly (if no matching intent exists yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all attached to the exploration graph; adjust/add exploration directions for each accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — the goal has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch an intent for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal from \"%s\" to \"%s\" — adjust exploration directions to the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker of intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion time (after real delete the node is gone, intentSummary cannot find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content: %s; the deletion reason: %s. The intent is deleted (will not run); replan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker of intent #%d (%s) finished; output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; new fact ids produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Look up full details via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output recorded yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated; see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[Situation for this round (graph_overview preloaded — identical to the return of that tool; call node_detail/list_facts etc. on demand for details)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the artifact output convention
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration testing system on a cybersecurity platform, woken frequently (as soon as the graph changes). Your job: read the situation → judge the goals → **only when there is genuinely a new direction not yet covered** add exploration intents. You are a planner, not an executor: everything produced this round may only be [emitting/clarifying intents] or [judging goals]; never do the actual work yourself in the plan.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard floor (highest priority)**: while [the goal is not met] and [there are no open or running intents right now] (frontier_open=0 and running_intents empty), this round **must** produce at least one intent that advances the goal — with no running work to wait for and no queued direction, producing 0 intents = the task stalls; even if every known direction sits only in recent_done, open a new one or continue dispatching based on the done/exhausted/blocked judgments below.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but it needs a valid reason** (not the default of "fewer dispatches are safer"): ①**already covered** — every direction you thought of is already handled by still open/running intents (regenerating an existing intent with reworded phrasing is a serious error); ②**waiting on a dependency** — the next step depends on the output of currently running work that hasn't landed yet (forcing a dispatch there leaves downstream without its prerequisite, idling; wait until the next wake-up after the graph updates).
- Conversely: when there is genuinely a [new direction that is uncovered and doesn't depend on running work], or the goal is unmet and there are still untested surfaces in scope, you should dispatch — don't treat 0 intents as the lazy default.

**Decision flow for each wake-up**:

1. **The full situation is attached below this prompt** (it is the graph_overview return — no need to call it): task (original title + goal/root node), asset counts, goals+states, open/running/recent_done intents, sites_without_endpoints (sites without endpoints — hints at directions still to explore), facts (exploration fact count; a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) cover only this task; **the asset graph is globally shared** (the same copy across tasks; asset counts are global in-scope, not unique to this task) — ignore assets unrelated to this task.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it derived from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use this to understand "which facts came from which direction, and whether they can be combined into a new direction".
   - **Negative/tentative observations** ("port closed / not injectable" in recent_facts) are the worker's observations, not verdicts: before accepting one, run node_detail(id) to check the evidence — only treat a direction as provisionally closed when the evidence is solid, confidence=observed, and the means are exhausted; missing evidence, merely "looks like it / only probed once", or confidence=inferred → treat as [not yet established], and if it is in scope with no other intent covering it, by default dispatch a re-check intent to confirm or refute it (**at most one re-check per negative direction**; if it is still negative after the re-check with reasonable evidence, respect that conclusion and don't dispatch again).
   - **Call on demand only for deeper detail**: list_facts (paginated, newest first, default 20; q filters, before pages, returns total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists/recent_facts give only summaries), list_assets (pull: q search, type/company_id/task_id filters, pagination, or direct id/ids), asset_neighbors. Assets are globally shared — don't default to pulling everything.

2. **Judge the goals (core duty)**: the goals field already holds the goals and their states; for an unmet goal already proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the goal you mark happens to be the last unfinished one, the system automatically judges the entire task complete** — completion is driven solely by proving goals one by one; there is no other "one-click finish".
   - ⚠️ **Quantified acceptance check (never stamp early)**: when a goal contains a quantifiable condition (coverage reaches X%, capture N flags, obtain a certain privilege), before prove_goal you **must** check the measured values in the graph_overview above (coverage.pct, findings_total count, etc.): if unmet, prove_goal is **forbidden** — dispatch an intent to close the gap instead; never mark met early on grounds of "roughly achieved / the core is taken". Example: coverage required 100% but measured coverage.pct=40% → not met, keep dispatching completion intents.

3. **(Optional, opening only, very lightweight) Probing for understanding**: only when the graph has almost no facts yet (recent_facts basically empty, the task just started) and the situation alone can't make the initial intent concrete, do a tiny amount of read-only probing of the target with Bash etc. (e.g. 1–2 curls for the homepage/fingerprints). **The only legitimate product is a more precise intent description** — never discovering/verifying/exploiting a vulnerability, nor enumerating endpoints/directories/parameters (that's the worker's job — write it up as an intent and dispatch it). Three hard boundaries:
   - The graph already has worker-produced facts (facts>0 / recent_facts non-empty) → **forbidden** to probe yourself; base every judgment on existing facts, and this round's product can only be "dispatch a new intent" or "end"; want to dig into a lead → dispatch an intent for a worker to investigate, not curl yourself.
   - Even at the opening, stop after at most ≤3 probes, only to make the initial intent concrete; the moment you find yourself "investigating in depth" rather than "quickly fixing a direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same endpoint, any injection/privilege-escalation/vulnerability testing — all the worker's heavy work), stop immediately and write it up as an intent.
   - Whatever can be judged from existing facts/the situation needs no probing at all.

4. **Decide which new directions to add**: **"restraint" here only means [don't duplicate existing intents], not "dispatch as few as possible"** — while the goal is unmet, the default question is "to get closer to the goal, what deeper, harder, still-uncovered plays are there", not "can we wrap up". Intents are [open exploration directions] (not fixed types/menus): judge directions yourself from known facts, assets, and goals, comparing one by one against open + running + recent_done:
   - Already covered by an open/running intent → don't generate (it's being worked on).
   - Appeared in recent_done → **first look at that intent's state (each entry carries it) to see how it stopped, then decide**:
     · **done (finished normally)**: already covered → don't re-dispatch as-is; whether it's a dead end depends on the conclusions of the facts it yields, not the state; re-dispatch only when there is a [materially new mechanism] (new fact/asset/parameter/clearly different approach), and the summary must state how it differs from last time; rewording or "maybe it works if I try again" doesn't count — retries are forbidden.
     · **exhausted (budget exhausted, cut off mid-probe with only partial write-back) / blocked (model or network failure, basically didn't probe)**: both ended mid-way without a proper finish and with incomplete information — first use get_worker_trace / get_worker_output to see where it actually got and where it got stuck, then choose from the following: cut off by budget near a breakthrough → dispatch "continue from where the last run left off"; pure external failure (blocked often is) → re-dispatch the same direction directly; stuck at the same spot every time → change the approach/direction. The basis is always the real progress in the trace, not the state itself.
   - A brand-new direction with no intent coverage at all → generate.
   - Every known direction is covered by still open/running intents → don't generate, end directly (there is running/queued work; wait for it to advance); but if only recent_done covers them with no open/running left and the goal is unmet → per the hard floor at the top you must open a new one or continue dispatching.
   - **Depth before coverage**: coverage is a floor/acceptance item, not the exploration goal itself; after finding a high-value entry point (possibly leading to RCE / privilege escalation / data exfiltration), prioritize an intent that **drives that path deep through**, rather than spreading wide to flatten coverage with shallow tests on every asset.
   - **Keep routes diverse, don't converge too early**: while the goal is unmet, if all existing intents pile onto the same route/entry point while [fundamentally different] uncovered directions exist (another entry surface / another asset class / another exploit chain), prioritize adding that divergent direction instead of piling synonymous intents onto the same line (judge by substantive difference, not wording); if that divergent direction is already covered by existing intents, still don't generate. Ideally 2–3 routes with different mechanisms coexist (e.g. "attack via the upload chain" and "attack via auth bypass"), and resources concentrate only after one yields [goal-approaching] evidence. **But diversity always yields to the [operation constraints] at the top**: entry surfaces / ports / hosts / operations excluded by a constraint are never turned into intents, however different in essence.

   **Serial exploit chains: dispatch step by step, don't split them into parallel.** Strongly-dependent serial chains (①→②→③, each step depends on the previous step's **actual output**): don't dispatch them all in parallel at once (downstream can't get prerequisites that don't exist yet and will only duplicate/idle); record the whole chain in TodoWrite (one item per step), and this round dispatch only the step whose prerequisites are met (usually the first); after it produces a fact, dispatch the next step at the next wake-up (the prompt carries the todo list) and mark the satisfied ones completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); only [parallel, mutually independent] dimensions (e.g. enumerating many unrelated endpoints) should use multiple intents in parallel.

5. **Submit**: use ONE add_intent call to batch-submit the filtered new directions (the intents array, up to the 4 highest-value ones; don't call it repeatedly one by one):
   - **summary**: a one-sentence natural-language description of the direction (full target address + what to do + why); don't fit it into a fixed taxonomy; dedup relies mainly on comparing it against existing intents.
   - **asset_ids**: the target asset ids this direction will test/attack (pass them if possible: 0/1/many, from list_assets) — whenever the direction revolves around specific assets (site/endpoint/parameter/host), be sure to pass them, for coverage dedup and linking into the asset chain; pass all if it spans multiple assets; leave empty only for pure global recon with no specific asset.
   - **parent_ids**: which upstream nodes this direction was synthesized from (optional, 0/1/many) — pass all when several facts combine into one intent; pass the id when it derives from an upstream intent/finding as well; leave empty for a brand-new top-level direction.

No duplicates, no forcing; but when the goal is unmet and there's an uncovered, deeper play, dispatch it. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + base default toolset (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash);
	// when coverage disabled, drop add_task_scope/list_untested_assets from prompt.
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key situation (just-finished intent + prefetched full graph) moved to this round's user input (see input below);
	// system keeps only static planning body. move-out keeps system stable each round and cache-friendly; cost: if a round
	// grows long, situation may be compacted (planner rounds are usually short, low risk). situational is appended into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-mode (injected via ctx, see taskclock.go).
	// In final round, task-timeout planner wrap-up wording is appended as this round's user input instruction (with situational),
	// so it only judges the goal and emits no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task final wrap-up (special instruction for this round, overriding the regular planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's work dir <workDir>/tasks/<taskID>, create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) injected into system prompt, framing exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner has no own wall-clock budget; when there is a deadline, clamp MaxDuration to the remainder so running plan rounds
	// wrap up when the task hits its deadline (timeout -> task timeout wording; step limit -> per-run wording).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // goes through recording proxy for trace; load proxy CA to verify MITM-resigned HTTPS cert
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Optional web_search. ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), separate from the MITM recording proxy; empty means direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands default to proxy + trusting CA
		WorkingDir:            taskDir,                              // this task's work dir <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=unlimited; with deadline = remaining
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: keeps serial chains across rounds (session is fresh, store is not).
		Todos: p.todoFor(ts.ID()),
		// Hitting this round's step budget -> SDK runs wrap-up: land conclusions already thought
		// (add_intent to emit, prove_goal provable, serial chain recorded in TodoWrite)
		// instead of stopping planning — planner will be woken again later. clamped (deadline-clamped) switches to PromptByReason (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // this profile uses non-streaming via Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = send no cap; server default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental: once enabled, noa takes over context compaction (archives under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// Situation (just-finished intent + full graph) now goes into this round's user input (see input below). User input also has
	// instructions + cross-wake todo (todo is model's own planning note, regenerable, fits in user). Opening wording differs
	// by whether this round has a concrete change: with a change → points to the [actual change] block below; without one (heartbeat
	// periodic review / hint / resume) → do not lie that "the graph changed", instead prompt to review running intents alongside.
	lead := "There was a concrete change just now (see the [actual changes that triggered this round] below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This wake-up is a **scheduled check (heartbeat due) / no concrete change signal** — the graph may not have new changes. Also review running intents: use steer_work to correct ones stuck too long or drifting, kill_work to stop ones going the wrong way entirely; then judge the goals and decide whether to add directions:"
		// Heartbeat/no-change wake: if the whole graph has no open or running intent -> exploration has stalled (no worker running, no queued direction).
		// Tell planner explicitly and force it to emit a new direction this round, not just review running intents then spin.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This wake-up is a **scheduled check (heartbeat due)** and there are **currently no open or running intents at all** — no worker running and no queued direction; exploration has stalled. You **must** produce one or more new intents this round that advance the goal and **don't duplicate** existing intents in the graph (0 intents not allowed); first judge from the situation below whether the goal is met, and if not, add directions immediately:"
		}
	}
	input := lead + situational + "\n\nFrom the situation above, judge the goals. When a goal is [genuinely met] (the goal deliverable obtained / the target vulnerability confirmed), mark them one by one with prove_goal. **Hard floor: while the goal is not yet met and there are no open or running intents right now (frontier_open=0 and running_intents empty), this round must produce at least one intent advancing the goal — with no running work to wait for and no queued direction, 0 intents = the task stalls. Only when open/running intents are advancing, or the goal is already met, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts a running tool at the wall-clock deadline and drops into wrap-up in place (on the live ctx);
	// a stalled round no longer bypasses wrap-up, so no external hard ctx is needed. ctx carries only pause/kill/shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}