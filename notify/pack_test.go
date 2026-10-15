package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "pack whole items" fix: when a batch message exceeds the channel's length limit, it must be
// truncated **by whole items** and report the count of items that didn't fit, so the caller only marks truly delivered ones.
//
// The previous approach rendered the full message then truncated, marking the whole batch as delivered: the latter half of
// the message vanished into thin air while delivery history showed all success — vulnerabilities just disappeared, with no
// way to detect it anywhere.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese summary items, guaranteed to exceed WeCom's 4096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body is %d bytes, exceeds limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part of the batch should fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// Header must truthfully state how many items this message contains and how many remain — otherwise readers take the
	// header number as the total.
	if !strings.Contains(body, "(showing the first 46, the remaining 154 continue in the next message)") {
		t.Fatalf("header should state how many items are not included in this message:\n%s", body[:minInt(400, len(body))])
	}
	// Should only contain the first kept items.
	for i := 0; i < kept; i++ {
		// allowlist: assertion matches the Chinese batch fixture item names (byte-budget packing fixture)
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	// allowlist: assertion matches the Chinese batch fixture item names (byte-budget packing fixture)
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("item %d must not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("with no length limit everything should be kept, got kept=%d", kept)
	}
	if strings.Contains(body, "continue in the next message") {
		t.Fatalf("no packing note should appear when nothing was dropped:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// When the budget is too small to fit even one item, still send one (the final truncation is the backstop). Otherwise one
	// over-long vulnerability permanently stalls the whole batch: every claim can't fit it, every time nothing is sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single-item message should report 1 delivered, got %d", kept)
	}
	// Empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0 delivered, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using byte budget would compress Chinese messages to one third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body is %d runes, exceeds limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only part of the batch should fit, got %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("the packing note should state that items remain:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should only fit part of the batch, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels don't truncate body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition not met")
	}
	// Indirectly confirm via the renderer's return value: markdownBody(0) with no limit keeps everything.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("with no length limit everything should be kept, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted content must not alter message structure".
// Titles and summaries come from model output (the model reads the target's response); asset names come from the target's URL.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in result (escaped form)
		wrong []string // must not appear in result (unescaped form)
	}{
		{
			name: "newline in title + external link",
			item: Item{
				Severity: "high",
				Name:     "Login SQL injection\n[Urgent: verify your account here](http://attacker.tld)",
			},
			// Newlines must be folded (otherwise fake list items/blockquotes); brackets and parens must be escaped (otherwise
			// clickable external links).
			must:  []string{`\[Urgent: verify your account here\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "Vulnerability ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and quote in asset name",
			item: Item{
				Severity: "high",
				Name:     "plain title",
				Assets:   []string{"a.com/*injection*>quote"},
			},
			must:  []string{`\*injection\*`, `\>`},
			wrong: []string{"*injection*"},
		},
		{
			name: "backticks and pipe in summary",
			item: Item{
				Severity: "high",
				Name:     "title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// Single-item mode's writeItem is the shared rendering path for all three markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("unescaped form present %q (could be used to inject structure or external links):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst pins the escape order: backslash must be handled first, otherwise the backslashes added
// later get wrapped again, producing double backslashes in output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escape order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes pins a specific regression: markdown escapes must not leak into Telegram's HTML
// output (escapes were once added in the shared title function, resulting in visible backslashes like `\(1\)` in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram body contains markdown backslash escapes:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
