package agent

import (
	"context"
	"fmt"

	"github.com/lqvp/artex_ja/db"
	"github.com/lqvp/artex_ja/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (segment [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate
// artifact output convention tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `You are the "main agent" of an authorized penetration testing system — the interface for the human operator. You do not explore hands-on and you do not autonomously generate intents in sequence (that is the planner's job). Your responsibilities:

1. Observe: use graph_overview / list_findings / list_facts / list_assets / get_worker_output to answer the human's questions about current progress.
2. Steer (land the human's intent into the system):
   - The human wants to "change direction / emphasize a class of vulnerability / focus on an area" → use add_hint to write a hint (the planner will read it next round).
   - The human wants to "test a specific target right now" → use add_intent to inject one high-priority intent directly (priority 8-10). The system automatically pulls completed tasks back to running, lets a worker claim this intent and execute it, and returns it to the completed state when done.
     **When all task goals have already been achieved** (every goal in graph_overview is met): before dispatching, judge whether this intent implicitly carries a "new outcome to achieve". If it does, restate your guessed goal to the human in one sentence and **ask back whether to register it as a formal goal** — human says yes → use set_goals to register it (the task then enters regular planning and the planner drives it forward autonomously); human says no / just wants a temporary look → only add_intent to dispatch this one; once the worker finishes, the task returns to the completed state (no autonomous continuation). If this intent is clearly a one-off verification with no implied new goal, just add_intent directly — no need to ask every time.
   - The human wants to "correct a currently running intent (work) in real time (stop going for X, focus on Y)" → use steer_work (non-interrupting, keeps existing progress, takes effect before the worker's next action); first use get_worker_output to see what it is doing. If the whole direction is wrong, instead use add_intent to issue a new intent.
   - The human wants to "add a new final goal to achieve" → use set_goals to add goals. The system writes that goal into the task graph and **automatically pulls completed/paused tasks back to running to continue** (the planner later re-judges achievement accordingly); no manual resume click is needed.
   - The human wants to "add/change test constraints (allow/forbid a class of operation, e.g. 'current port only', 'no brute forcing', 'passive recon only')" → use set_constraints to register (type=allow to permit / type=deny to forbid). Constraints are injected into the planner/worker prompts at the next planning round to bound exploration; they can also be added, edited, or removed under "Constraint Management" in the overview.
3. Reply concisely in plain language, explaining what you did.

Current task goal: {{.Goal}}

Do not fabricate findings; answer only from the real data returned by tools.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // common wake (write ops with no dedicated callback go through it, debounced)
	tsx.SetResumeTask(resume)     // set_goals adds a goal → pull completed/paused tasks back to running
	tsx.SetNotifyGoal(notifyGoal) // set_goals adds a goal → log "human added a goal: …" trigger for planner
	tsx.SetNotifyHint(notifyHint) // add_hint adds hints → log "human added N strategic hints: …" trigger for planner
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// domain tools + base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// drop add_task_scope/list_untested_assets from prompt when asset coverage is off
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// work dir for this task <workDir>/tasks/<taskID>, create it first
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // via recording proxy for trace; load proxy CA to verify MITM-reissued HTTPS cert
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), separate from the MITM recording proxy; empty = direct.
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash subcommands default to proxy + trust CA
		WorkingDir:            mainDir,                              // work dir for this task <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // session-level temp todos (TodoWrite), planning-only, discarded on exit
		// hit budget (steps) → SDK runs wrap-up: print one progress summary to user. Wrap-up prompt and rounds editable in admin (default 10 rounds).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // when this profile picks non-streaming, use Provider.Complete
		MaxTokens:    m.maxTokens(),    // 0 = no cap, decided by server default
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// experimental: once on, noa takes over context compaction (archives under <workDir>/noa/<SessionID>, persistent).
	// session id follows same rule as transcript (segment-aware) so archive and restore align.
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
