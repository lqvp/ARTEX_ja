package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset is the checksum manifest release.yml generates, covering every zip in the Release.
const sumsAsset = "SHA256SUMS"

// maxBinarySize caps the extracted binary size, so a malformed zip cannot fill the disk.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase is the upgrade stage, used directly as the SSE event's phase field.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress is supplied by the caller to push progress to the frontend. pct is meaningful only
// during the download phase (0-100); pass -1 for the other phases.
type Progress func(ph Phase, pct int, msg string)

// Stage downloads the specified Release's package for this platform, verifies, and stages as artex.new.
//
// It uses the full zip, not a bare binary: SHA256SUMS covers only the zip, so no CI change is
// needed and already released versions stay compatible; the zip also carries skills/ for later.
// Returning means staging is complete; the caller then shuts down gracefully with ExitRestart.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("this release has no package for %s/%s (missing %s)", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "Fetching checksum manifest…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s does not list %s; refusing to install an unverified binary", sumsAsset, name)
	}

	// All temp files live in the target directory, so the final rename is an atomic operation
	// within one filesystem (cross-device rename fails; /tmp is often a separate mount).
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("Downloading %s (%s)…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "Verifying SHA256…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 mismatch: expected %s, got %s (download corrupted or tampered)", short(want), short(got))
	}

	prog(PhaseExtract, -1, "Extracting and smoke testing…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("new version cannot run on this system: %w", err)
	}

	// The staged file's own sha256 is stored separately: before the next swap it is
	// re-checked, so the file cannot be altered or corrupted after staging.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("computing new binary checksum: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("writing checksum: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("staging new version: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// The marker only affects auto-rollback; the staged file is in place, so this does not abort the upgrade.
		prog(PhaseStaged, -1, "Warning: failed to write upgrade marker; this upgrade has no auto-rollback protection")
	}

	prog(PhaseStaged, 100, "New version ready, restarting…")
	return nil
}

// fetchSums downloads and parses SHA256SUMS, returning filename -> hex digest.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("this release has no %s; cannot verify integrity, refusing to upgrade", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty or has an unrecognized format", sumsAsset)
	}
	return out, nil
}

// parseSums parses a sha256sum-style manifest, returning filename -> hex digest.
//
// The first field must be 64 hex digits to be accepted. "exactly two fields" alone is not
// enough: any two-word sentence would be treated as a valid entry, polluting the digest
// table and possibly matching the real asset to the wrong digest.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// "sha256sum" uses two spaces between fields; shasum in binary mode prefixes
		// the filename with *.
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download writes the asset to dst, computing SHA256 and reporting progress by Content-Length.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("download interrupted: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("flush to disk failed: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("incomplete download: expected %d bytes, got %d bytes", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get issues a allowlist-constrained GET and returns the response body.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary pulls the artex executable out of the release package.
//
// The package layout is artex-<version>-<os>-<arch>/artex, but this matches by base
// name rather than the full path: the version appears in the package name, and
// one typo breaks every platform's update. Base-name matching is more tolerant.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("opening release package: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("writing out new binary: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("extracting %s: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("executable in release package exceeds %s; refusing to extract", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("%s in release package is empty", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("%s not found in release package", want)
}

// checkWritable confirms the directory is writable up front. Without it, a non-root run or
// a binary in a system dir would download tens of MB before failing at swap time.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("program directory %s is not writable; cannot auto-update (check permissions or update manually): %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter counts bytes written and reports throttled, so every 32 KiB chunk
// does not push an SSE event.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("Downloading %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
