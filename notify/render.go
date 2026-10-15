package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to no more than max bytes, guaranteeing valid UTF-8.
//
// WeCom markdown has a 4096 **byte** hard limit (not characters), and one Chinese
// character is 3 bytes. Slicing by bytes cuts a character in half, producing
// invalid UTF-8. The approach: roll back from the budget position to the nearest
// rune start byte (utf8.RuneStart identifies continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. After truncation, append ellipsis unless max is too small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than ellipsis: give up ellipsis, pure truncation.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine collapses multi-line text to a single line: folds all whitespace, then
// truncates by character count. Used for IM message titles — summaries often
// contain newlines, and stuffing them into tables/titles breaks layout.
// max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to no more than max characters (not bytes), appending
// ellipsis when truncated. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's measurement: WeCom limits
// by bytes, Telegram by character count. Using the wrong measure won't error, but
// will make messages cut far shorter than expected (1 Chinese char = 3 bytes;
// cutting 4096 by bytes leaves only ~1365 chars), so both functions must be
// preserved and selected per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count, guaranteeing no
// half-cut tags.
//
// Directly character-truncating HTML produces broken tags like `<a href="htt`;
// platform parsers either reject the entire message or swallow subsequent text as
// attribute values. The approach: first truncate by characters, then check
// whether the tail has an unclosed `<`, and if so, roll back to before it.
//
// Tag balancing is not done: Telegram's HTML parser auto-closes unclosed tags,
// while implementing balancing requires handling quotes in attributes, comments,
// self-closing tags — complexity disproportionate to the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a `<` fragment (last `<` has no `>` after it), roll back.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a cut-off HTML entity (e.g. `&amp;` cut to `&amp`), roll back.
	// An entity fragment in an entity-only parser may cause the **entire message**
	// to be rejected — an over-long batch message is already common.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount calculates how many items fit **completely** within the budget.
//
// Why by whole items: truncation makes items vanish while their delivery records
// still show as delivered — vulnerabilities just disappear. After whole-item
// packing, items that don't fit stay in the DB as the next batch.
//
// Parameters: maxSize<=0 means no limit; reserve is for message header/footer;
// size handles measurement (WeCom/DingTalk by bytes, Telegram by characters);
// render renders the idx-th item to its actual text. Returns at least 1.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two measurement modes for packItemCount.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders an asset list as a single display line, omitting the rest and
// annotating the total count when exceeding limit items.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " and " + itoa(len(assets)) + " total"
}

// itoa is a short alias for strconv.Itoa, only for concatenating display text.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
