package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lqvp/artex_ja/db"
)

// Rendering for the Findings-page "Export": renders a batch of findings rows as
// consolidated Markdown, single Markdown, or CSV. JSON is serialized from DTOs by the server layer, not here.

// sortFindingsForExport orders by severity descending, then time descending, matching the summary report's grouping.
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // lower sevRank = more severe
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle returns the finding's readable title: name → class → "Uncategorized".
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "Uncategorized"))
}

// FindingsMarkdown merges a batch of findings into one summary report (summary +
// grouped by severity, each entry carrying class/status/owning task/evidence/full report).
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# Findings Summary Report\n\n")
	fmt.Fprintf(&b, "- **Generated at**: %s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **Total findings**: %d\n\n", len(items))

	// Summary: counts per severity level.
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## Summary\n\n")
	b.WriteString("| Severity | Count |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "Critical"}, {"high", "High"}, {"medium", "Medium"}, {"low", "Low"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_No matching findings._\n")
		return b.String()
	}

	b.WriteString("## Findings Detail\n\n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **Class**: %s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **Status**: %s\n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **Owning task**: %s\n", desc)
		}
		fmt.Fprintf(&b, "- **Found at**: %s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**Evidence:**\n\n```\n%s\n```\n\n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("**Full report:**\n\n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown renders one finding as a standalone Markdown (for the "one vulnerability, one file" packaging).
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **Class**: %s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **Severity**: %s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **Status**: %s\n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **Owning task**: %s\n", desc)
	}
	fmt.Fprintf(&b, "- **Found at**: %s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **Generated at**: %s\n\n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## Overview\n\n%s\n\n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## Evidence\n\n```\n%s\n```\n\n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## Full Report\n\n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename builds a safe .md filename for the "one vulnerability, one file" scheme,
// shaped like `critical_SQL_injection_#123.md`. Strips path separators and control characters to avoid illegal paths inside the zip.
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// Defensive: strip one more path layer to eliminate zip slip.
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// FindingsCSV renders a batch of findings as CSV (with a UTF-8 BOM so Excel correctly
// recognizes the Chinese text). It omits full report/evidence text and keeps only summary fields; use the Markdown/JSON exports for full text.
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "Name", "Class", "Severity", "Status", "Owning task", "Found at", "Summary", "Traffic evidence count", "Traffic evidence IDs"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			findingTitle(f),
			f.VulnClass,
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			f.TaskDescription,
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			strings.TrimSpace(f.Summary),
			fmt.Sprint(len(f.TrafficBindings)), findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## Traffic Evidence\n\n")
	fmt.Fprintf(&out, "Evidence version: %d; bindings: %d.\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("Evidence has changed; full report pending update.\n\n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **Evidence #%d · %s** — `%s %s`, status code %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [Request message](evidence/%d/%d/request.http) · [Response message](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
