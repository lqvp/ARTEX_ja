package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most critical invariant in this package. WeCom limits by **bytes**, Chinese is 3 bytes/char, and any
	// byte-level hard cut splits Chinese characters in half, producing invalid UTF-8 that platforms reject. Use multiple
	// Chinese-English mixed inputs with coprime lengths to hit every possible cut point.
	inputs := []string{
		// allowlist: multibyte fixture exercises byte-budget truncation
		"中文测试内容",
		// allowlist: mixed multibyte fixture exercises byte-budget truncation
		"混合 mixed 内容 content",
		// allowlist: multibyte fixture exercises byte-budget truncation
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4-byte emoji, more obvious when cut wrong
		// allowlist: multibyte fixture exercises byte-budget truncation
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result is %d bytes, exceeds limit", in, max, len(got))
			}
			// When not truncated, content must not be modified.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: under limit but content changed -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean unlimited")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean unlimited")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than ellipsis itself, appending ellipsis must not cause the result to exceed the limit instead.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("with max=1, result %q has length %d, exceeds limit", got, len(got))
	}
	// Normal case should include ellipsis.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The measurement difference from TruncateBytes must be preserved: Telegram limits by character count; using byte measure
	// cuts Chinese messages to one third.
	// allowlist: multibyte fixture exercises rune-vs-byte counting (10 CJK runes = 30 bytes)
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 runes, got %d (%q)", n, got)
	}
	// The same string measured in bytes should be noticeably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("byte-based measure must not produce the same rune count as rune-based measure")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("line one\n\nline two\twith tab   multiple spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("all whitespace should be collapsed, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not survive, got %q", got)
	}
	// After truncation it must still be readable and valid.
	// allowlist: multibyte fixture exercises rune-count truncation (Chinese would be cut mid-character by byte measure)
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 runes, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Directly truncating HTML produces fragments like `<a href="htt`; platforms reject the entire message.
	s := `<b>Title</b>text text text<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result is %d runes, exceeds limit", max, n)
		}
		// Tail must not have an unclosed `<` (i.e. `<` appears in the last segment but no `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: trailing tag was cut in half -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should yield empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("under the limit all assets should be listed, got %q", got)
	}
	// When exceeding the limit, the total count must be annotated, otherwise readers don't know how many assets are not listed.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "and 5 total") {
		t.Fatalf("total count 5 should be annotated, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("empty severity ranks lowest and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("empty threshold should let everything through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("known status should map to its label, got %q", got)
	}
	// Unknown status is echoed as-is, no fabricated label.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be echoed as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers a gap flagged in audit: truncation must avoid not only half-cut tags but also
// cut-off HTML entities.
//
// After `&amp;` is cut to `&amp`, an entity-only parser may reject the **entire** message — and over-long batch messages
// are already common, too high a price.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// Tail must not have an entity fragment ("has & but no corresponding ;").
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: trailing entity fragment left behind %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity produced", max)
		}
	}
}
