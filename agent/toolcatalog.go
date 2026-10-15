package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns built-in tools from pure code into an enumerable catalog overridable by DB:
//   - BuiltinToolSeeds(): expands the 3 execution agents' built-in tool sets into seed records (key +
//     desc + param schema + default bound agent), idempotently seeded into tools table at startup.
//   - ToolResolve hook: at runtime filters/assembles tools per DB rows (filter by agent +
//     override desc/schema + inject default params). key/handler stay in code; DB only edits prose/defaults.
// handler (Call behavior) is always from code — DB cannot change it, only model-facing desc and defaults.

// ToolSeed is a seedable snapshot of a built-in tool: key = CoreTool.Name() (bound to handler,
// UI read-only), Desc/Schema from tool definition in code, Agents = default bound agents.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), PK, immutable
	Desc   string         // top-level description (overridable in UI)
	Schema map[string]any // param JSON-Schema (structure read-only; desc/default editable in UI)
	Agents []string       // default bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent builds each execution agent's domain toolset via a "read-only empty shell"
// ToolSet (nil stores); constructors only put closures into Spec, no store deref at build, nil-safe.
// these tools here only read Name()/Description()/InputSchema(); never Call.
//
// deliberately excludes SDK common tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): each agent owns them fixedly, no binding choice, and docs mostly in Prompt()
// (this table only covers Description(), partial coverage would mislead). no seed -> no DB row -> ToolResolve
// passes through unchanged. only artex's own domain tools enter the table.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (goal decomposer) defaults to set_goals + set_constraints: writes decomposed goals,
		// extracted constraints to DB; shares managed tools with mainagent; web can edit desc/schema and toggle per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto defaults to vuln reporting + asset management tools; other domain tools toggleable in UI.
		// new store seeded here; old store migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone pentest agent) defaults: list/insert assets, report/list vulns, list companies.
		// new store seeded here; old store migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools still enter the catalog (visible on web, manually bindable per agent), but
// by default bound to no agent - ToolResolve discards empty-bound tools for all agents; explicit opt-in required.
// they remain in some agent's base toolset (e.g. goal_met in PlannerTools) so seed can
// build it to get desc/schema, and so ToolResolve can keep it once user manually binds it back.
//
// goal_met: bypass per-goal prove_goal and declare the whole task done globally; high-weight, misjudge-prone, also
// duplicates "prove_goal marks last goal -> auto wrap-up", so not bound to any agent by default; bind manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds dedup-merges each agent's built-in toolset into a seed list: same-named tools (e.g. list_assets
// present in multiple agents) collapse into one, Agents = union; defaultUnbound tools forced to empty binding.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // enter catalog, manually bindable, but no agent by default ([] not null, consistent with others)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// default params get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only defaults are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
