package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, type-mismatched fields must all degrade to the zero-value Filter, i.e. "no filter". This invariant is the landing
	// point of "prefer over-delivering to dropping": once this changes to returning errors or half-parsing, a single misconfigured
	// character would silently drop all high-severity notifications.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top level is an array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("malformed config should degrade to the zero-value Filter, got %+v", f)
			}
			// A zero-value Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("zero-value Filter should match every event")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// An unknown severity ranks 0 and should be blocked by any non-empty threshold (when in doubt, do not push).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: want %v got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope = unrestricted", Filter{}, true},
		{"task hit", Filter{TaskIDs: []int64{7}}, true},
		{"task miss", Filter{TaskIDs: []int64{8}}, false},
		{"multi-task selection includes hit", Filter{TaskIDs: []int64{8, 7}}, true},
		{"assets intersect", Filter{AssetIDs: []int64{20, 99}}, true},
		{"assets disjoint", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both hit", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task hit but asset miss", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("want %v got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"empty include = accept all", Filter{}, "any class", true},
		{"include hit", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include miss", Filter{VulnClassInclude: []string{"command execution"}}, "SQL injection", false},
		{"include multi-keyword any hit", Filter{VulnClassInclude: []string{"command execution", "SQL"}}, "SQL injection", true},
		{"case-insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL injection", true},
		{"exclude hit wins", Filter{VulnClassExclude: []string{"info disclosure"}}, "info disclosure", false},
		{"exclude miss passes", Filter{VulnClassExclude: []string{"info disclosure"}}, "SQL injection", true},
		// Exclude takes precedence over include: disqualifies even when an include list also matches.
		{"exclude takes precedence over include", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"injection"},
		}, "SQL injection", false},
		// Pure-whitespace keywords must be ignored, otherwise every string containing a space would match.
		{"blank keywords ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("want %v got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Default off: most users mean "newly discovered vulnerability" when they say "push", not a running log of every state transition.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("status-change events should be skipped when not enabled")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("status-change events should match once on_status_change is enabled")
	}
	// Create events are unaffected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("create events must not depend on on_status_change")
	}
}