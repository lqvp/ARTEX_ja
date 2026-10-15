// Package notify implements the IM / email push channel adapter layer for vulnerability findings.
//
// Layering: this package is a **leaf package**, depending only on the standard library. It knows nothing
// about databases or the server. Channel config is passed in as map[string]any (corresponding to the
// notification_channels.config JSONB column), and content to push is passed as Message. The benefit of this
// separation is that signature computation, UTF-8 truncation, and filter matching — the truly error-prone
// parts — can be unit-tested without PostgreSQL, and the host only needs to orchestrate on the server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The same Channel instance is reused
// concurrently by multiple channel configs (even multiple bot instances of the same channel); all credentials are
// passed in via the cfg parameter, and implementations must not cache things like webhook URLs in their own fields.
package notify

// Channel type identifier. Values are also the valid set for notification_channels.kind, validated by a
// server-side whitelist (same as findings.status — no DB CHECK, making it easy to add channels later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (incl. Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event type, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for empty kind in config.
const InitKind = KindDingTalk

// severityRank maps vulnerability severity to a comparable ordinal. Unknown severity returns 0, so any
// min_severity setting blocks unknown severity — when in doubt, don't push, avoiding false-positive spam.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal of the severity; unknown severity returns 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns a human-readable severity name+emoji for message title/card colors; unknown is echoed as-is.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates disposition status to a human-readable label, for status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast checks whether severity meets the min threshold. Empty min means no threshold, all pass.
// Note that unknown severity has ordinal 0 and is rejected by any non-empty min (see severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
