package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: filtering conditions for a channel instance. All fields are optional and default to "no filter" — the fallback semantics for malformed config; see ParseFilter.
type Filter struct {
	// MinSeverity is the lowest severity threshold (low/medium/high/critical), empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs as an empty array means unrestricted; non-empty requires the event to intersect.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude empty means accept all; non-empty requires vulnclass to hit one keyword. VulnClassExclude hitting any keyword excludes (exclude over include).
	// Matching is case-insensitive substring — safer than regex: a misconfigured regex cannot silently break a channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange decides whether the channel receives vulnerability status-change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses channel filter config.
//
// **Never returns an error.** This is a deliberate design choice: when filter
// config is malformed, degrade to the zero-value Filter (= no filter = match
// all), because delivering one extra message is far better than silently
// dropping a high-severity one; making parse failure mean "no delivery" would
// give a channel that looks configured but pushes nothing — the worst failure
// mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays at its zero value, i.e. no filter.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (empty
// string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate validates constrained-value fields in filter config, called at channel-save time.
//
// Why enforced on write: Match treats an unknown threshold as `rank >= 0`, always true — a min_severity typo ("hgih") silently disables the filter and turns it into "push everything". Consistent with "prefer over-delivering to dropping" (nothing dropped), but the user believes they are grading pushes while every vulnerability floods the group with no sign of wrong config. Such "silent degradation" must be blocked at the entry point.
//
// Note Validate is for the **write** path only; the read path keeps ParseFilter's tolerant semantics, so bad historical values don't make the whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid; choose: low / medium / high / critical, or leave empty for no limit", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel carrying
// these filter conditions.
//
// **Never returns an error**, for the same reason as ParseFilter: any internal
// abnormality is treated as "match". Decision order: event type → severity
// threshold → task/asset scope → vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events only reach channels that explicitly opt in. Default
	// off, because most users mean "newly discovered vulnerability" when they
	// say "push", not a running log of every state transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclude takes precedence: hitting any exclude keyword disqualifies even
	// when an include list also matches.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is enough for small sets; both sides are at most a few
	// dozen items hand-picked. Building a map costs more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}