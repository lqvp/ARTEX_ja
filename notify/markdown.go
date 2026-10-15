package notify

import (
	"fmt"
	"strings"
)

// This file is the shared message renderer for "Markdown-family" channels (DingTalk, WeCom).
// Feishu uses card JSON, Telegram uses HTML, email uses HTML — each rendered in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A vulnerability can anchor dozens of assets;
// listing them all floods the message with no informational value — nobody reads past the 4th domain in IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. IM messages are "a nudge to go read the detail",
// not the report itself; full content lives in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (summary line + severity distribution + possible truncation notice)
// and footer (platform link). Deducted from the budget when packing the whole message, ensuring head and tail are never cut —
// once head/tail are cut, the reader cannot tell "which batch this is, or how many items are not shown".
const markdownReservedBytes = 320

// markdownEscape escapes markdown metacharacters.
// Why this is necessary: vulnerability titles, summaries, types, and asset display names all come from
// **untrusted sources** — titles and summaries come from model output (the model reads the tested target's
// responses), and asset URLs are full URLs obtained from scanning (including target-controlled query strings).
// Without escaping, a vulnerability titled
//
//	login-form SQL injection\n[URGENT: click here to verify your account](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's DingTalk/Feishu; and
// `![](http://attacker.tld/beacon)` would be fetched by the client at render time, effectively reporting
// "this vulnerability has been viewed" and leaking the reader's IP. Even without malice, injected bold or
// blockquotes can push the severe vulnerability below the fold.
//
// The escape set covers characters in titles/links/emphasis/lists/quotes/strikethrough that change structure
// or produce clickable elements. `\` must be handled first, otherwise it re-escapes the backslashes added later.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText compresses untrusted text to a single line and escapes it, for use in markdown body.
// Single-lining is the other half beyond escaping: a newline alone can forge a new list item or blockquote,
// and escape characters cannot stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (IM platform title bar / card title); content is **unescaped raw text**.
//
// Deliberately does not escape here: this title is shared by four rendering contexts — markdown body, Telegram's
// HTML, Feishu card plain_text, and generic Webhook JSON and email subject. Each context has different
// escaping rules (markdown escaping stuffed into HTML leaves visible backslashes; stuffed into JSON it corrupts
// data), so escaping must be handled by each output end; see writeItem / feishuItemLines /
// telegramEscape. Markdown escaping was once added to the shared function, resulting in visible backslashes
// like `\(1\)` in Telegram messages.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Vulnerability summary · %d total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Vulnerability notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body, returning the body and the **actual number of items written**.
//
// The return value kept is the number of items actually delivered in this delivery; the caller uses it to mark
// only the first kept items as delivered — items blocked by the channel's length limit must be left for the next
// batch, not marked successful along with the rest. This is the source of "silent loss": the message was truncated,
// but the delivery record shows everything delivered, with no way to see the second half was never sent.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Single messages are sent even when oversized (backstopped by final truncation): partial information
		// about one vulnerability is better than sending nothing.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in the platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the digest message header: time window, count, and severity distribution.
// With these, the recipient can judge whether this batch needs immediate attention without clicking into the platform.
//
// items are the items **actually packed**; total is the expected total for this batch. When the two differ,
// it must state "how many more items are in the next message" — otherwise the reader assumes the number in the
// message header is everything, and the items that were never sent simply do not exist on screen.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**Last %d min: +%d vulnerabilities**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**+%d vulnerabilities**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(showing the first %d, the remaining %d continue in the next message)", len(items), extra)
	}
	// Gives distribution by severity so the reader sees at a glance whether there are critical items. Counts only
	// items **actually included in this message**, ensuring "critical 3" matches the items countable below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single vulnerability item.
//
// prefix is the sequence number for digest lists; single=true renders the full version (with summary and back-link),
// while digest lists render only a one-line summary — otherwise a 50-item digest becomes a long document.
//
// All externally-sourced content (title/type/assets/summary) goes through markdownText:
// single-line + escaping. The back-link is assembled from the admin-configured public_base_url, not untrusted content,
// and must be a clickable link, so it is output as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: single-line presentation, with assets and summary compressed and appended after.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Class**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
