// Package selfupdate implements one-click updates from GitHub Release: fetch, verify,
// stage the binary, and atomically swap it on next start.
//
// Overall division (see start.sh / start.bat):
//
//	start script = dumb guardian loop, only decides whether to relaunch by exit code
//	this package  = all error-prone logic (download / SHA256 / smoke / swap / rollback)
//
// Swap lives in Go rather than the script because sha256 + smoke tests would need
// two implementations (sha256sum/shasum/certutil) in sh and bat — and this is the
// one step where a mistake leaves the guardian faithfully restarting a dead binary.
//
// One full upgrade involves three process starts:
//
//	1. old server receives /api/update/apply -> download verify -> stage artex.new -> exit 75
//	2. script relaunches old -> Bootstrap sees artex.new -> verify+smoke -> swap -> exit 75
//	3. script relaunches, now new version -> Bootstrap records attempt -> clears marker after stable
//
// Any step failing reverts to old: stage 2 discards the staged file; stage 3 reverts
// artex.old after 3 failed starts.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code meaning "guardian, relaunch me" (EX_TEMPFAIL); the script
// relaunches immediately without crash backoff. 0 = normal stop; other codes = crash.
const ExitRestart = 75

// maxAttempts is the allowed start count after a swap. Each new-version start
// increments; clearing on settleDelay means stable run. Exceeding it triggers rollback.
const maxAttempts = 3

// Paths are all files involved in one upgrade, rooted at the executable's directory.
// Deliberately not CWD: in service mode the working dir may be / or anything else, so
// a staged file elsewhere would make swap logic fail.
type Paths struct {
	Dir     string // executable's directory
	Current string // currently running binary          artex / artex.exe
	New     string // staged new version              artex.new / artex.new.exe
	Sum     string // new version sha256 (hex)         artex.new.sha256 / artex.new.exe.sha256
	Old     string // backed-up previous version      artex.old / artex.old.exe
	Marker  string // upgrade state marker            artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows .new/.old must keep .exe, so strip the suffix first then
// reconstruct; this keeps naming symmetric across both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locating executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records swap progress, to trigger auto-rollback if the new version cannot start.
type marker struct {
	From     string `json:"from"`     // version before upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // number of starts after swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged file. Called after successful swap, failed
// verification, or cancellation, to avoid a leftover artex.new being retried.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two versions, returning -1/0/1 (a<b/a==b/a>b).
// ok=false when at least one side is not comparable (e.g. local "dev" build or
// git-describe "0.3.7-2-gabc1234-dirty"). Caller should disable one-click update
// then, otherwise a dev build could be "upgraded" and overwrite uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses "v0.3.7" / "0.3.7" into a [3]int.
//
// Only the clean three-part form is accepted: build.sh uses git describe for non-tag
// builds, producing "0.3.7-2-gabc1234". That must be judged incomparable, not treated
// as 0.3.7, or dev builds get misclassified as "already latest"/upgraded by a release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process runs in a container. Under Docker, swap writes to
// the container's writable layer; `docker compose up -d` reverts to the image's binary —
// expected (the user was pulling a new image anyway), but the UI needs to say so plainly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
