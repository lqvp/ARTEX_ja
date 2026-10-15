package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. Producer is the db layer's
// vulnerability storage transaction; consumer is the server layer's delivery engine and filter matching. Defined in this package because it is the
// "notification domain" payload: db only serializes it and does not understand field semantics.
//
// Fields are stored redundantly instead of re-queried at render time because a vulnerability can later be renamed, re-graded, or re-statused,
// while pushed content must reflect the conclusion at event time. Re-querying would yield the misleading "downgraded to low afterwards".
// Redundancy also means fan-out and rendering need no JOIN on findings/tasks/assets.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a vulnerability pending push, rendered by a channel.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets is the resolved asset display-name list (e.g. domain/IP). Filled by the server layer —
	// this package does not touch the database, so it cannot obtain the names.
	Assets []string
	// DetailURL is the vulnerability detail back-link; empty means public_base_url is unset and is omitted at render time.
	DetailURL string
	// Fields specific to status-change events; when both are non-empty the item renders as "pending → fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the item is a
// status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: a manually given name wins, then the vulnerability class vulnclass, and when both
// are empty a placeholder is used — never an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed vulnerability)"
}

// Message is the complete content of one channel send.
type Message struct {
	// Length is 1 for a single push; an entire batch for a digest push. An empty slice is invalid; the caller must guarantee at least one.
	Items []Item
	// When Batch=true render as a digest message (different title, plus time window and count).
	Batch bool
	// WindowMinutes is the digest period (minutes), used for the "last N minutes" wording only when Batch=true.
	// Deliberately passed explicitly via config rather than computed from time.Since at render time: rendering stays deterministic, so it is testable.
	WindowMinutes int
	// HomeURL is the platform panel address (global public_base_url); when empty no panel entry is included.
	HomeURL string
}
