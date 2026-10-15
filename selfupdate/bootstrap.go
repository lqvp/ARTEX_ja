package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv makes a subprocess launched by smoke tests skip Bootstrap.
//
// Strictly, leaving it unset is harmless: the subprocess is artex.new, so all derived
// paths carry .new and never touch real upgrade files.
// But relying on that coincidence is fragile; explicit short-circuit avoids it.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is the instruction Bootstrap gives main.
type Action int

const (
	// Continue: start the server normally.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the guardian relaunches.
	Restart
)

// State describes this start's upgrade status, so /api/update/check can tell
// the frontend whether the last upgrade succeeded or rolled back.
type State struct {
	Pending     bool   // not yet confirmed stable after swap
	RolledBack  bool   // auto-rollback ran during this start
	FailedStage bool   // staged file failed verification/smoke, discarded
	Detail      string // one-sentence user-facing note
}

// Bootstrap runs at main's very start, before any port is listened or DB opened.
//
// Three situations:
//
//	1) artex.new present -> verify + smoke, swap and restart on success; discard and keep running old on failure
//	2) marker only        -> record this attempt; roll back after enough consecutive failures
//	3) nothing            -> normal start
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles "staged file exists": swap on verification success, discard on failure.
//
// This is the one place the executable is overwritten; the smoke test is the last gate, stopping
// corrupted downloads, wrong architecture, missing libraries. Releasing a non-running binary here
// would have the guardian relaunch it endlessly; Go never gets to run, so auto-rollback never triggers.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged new version failed verification, discarded; continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "New version verification failed, discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed, continuing with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Swap failed: " + err.Error()}
	}

	// Swap succeeded. Keep the marker so the next start (now the new version) can confirm stability.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write upgrade marker (auto-rollback disabled): %v", err)
	}
	log.Printf("[update] switched to %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "after a swap": count attempts; exceed limit and roll back the old version.
//
// Counting only increments after Go can run, so it covers "executes but crashes during init"
// (config mismatch, port clash, DB migration failure); "not exec at all" is blocked by the
// pre-swap smoke test. Together they form the complete safety net.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// Rollback also failed: do not restart or it loops forever. Clear the marker so the process
			// starts in its current state; if it cannot start, the reason is visible in the logs.
			log.Printf("[update] new version failed to start %d times in a row, and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "New version failed to start and rollback failed: " + err.Error()}
		}
		log.Printf("[update] new version failed to start %d times in a row, rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("New version failed to start, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update upgrade marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d); upgrade will be confirmed once it runs stably",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms the new version runs stably, then clears the upgrade marker.
//
// Called by main after the HTTP listener is up with a delay: the process must survive
// this long to count; otherwise the marker stays and attempts keep accumulating.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // not an upgrade follow-up start; nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear upgrade marker: %v", err)
		return
	}
	log.Printf("[update] new version running stably, upgrade complete (previous version kept as %s)", p.Old)
}

// SettleDelay is the runtime length required to judge "new version survived".
const SettleDelay = 30 * time.Second

// verifyStaged validates the staged file: compare SHA256, then actually run it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("reading checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download corrupted or tampered)")
	}
	return smokeTest(p.New)
}

// smokeTest launches the new binary with -h to confirm it really can execute here.
// This blocks truncated downloads, wrong architecture (exec format error), missing deps, etc.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("setting execute permission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary unresponsive)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows blocks
// deletion/overwrite, not rename), so no platform split or self-stop is needed.
func swap(p Paths) error {
	// Windows rename does not overwrite an existing target; the leftover .old from the previous run must be removed first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("backing up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// Swap failed but the current binary was already moved: restore it as-is, otherwise next start has no executable.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("installing new version failed (%v), and restoring current version failed: %w", err, rerr)
		}
		return fmt.Errorf("installing new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps the backed-up old version back in.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no backup to roll back to at %s: %w", p.Old, err)
	}
	// Move the non-starting new version to .failed for forensics instead of deleting it.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("moving failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restoring old version: %w", err)
	}
	return nil
}

// Rollback is /api/update/rollback's implementation: actively revert to the previous version.
// It only swaps; restart is delegated to the guardian (caller then exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot execute, refusing rollback: %w", err)
	}
	// Swap current and backup: after rollback we can roll back again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("moving current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("installing previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to tidy backup after rollback (operation unaffected): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a rollback target exists, for the frontend's rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}
