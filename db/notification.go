package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lqvp/artex_ja/notify"
)

// This file is the channel config and event layer for IM push. Delivery-task claiming and state transitions
// live in db/notification_delivery.go.
//
// Two invariants to preserve when editing this file:
//
//  1. The vulnerability-writing transaction (RecordFindingTx) only calls InsertNotificationEventTx once as a
//     blind insert — it reads no notification-related tables and does no filter matching. Any read introduced
//     here can be polluted or aborted by a user's misconfigured filter conditions.
//  2. Filter matching never errors: a malformed config is treated as "match" (see notify.Match). Prefer over-
//     pushing to missing a push.

// ErrNotificationChannelNotFound channel does not exist.
var ErrNotificationChannelNotFound = errors.New("notification channel not found")

// Delivery states.
const (
	NotifyStatePending = "pending" // pending
	NotifyStateSending = "sending" // claimed by a dispatcher, lease not yet expired
	NotifyStateSent    = "sent"    // delivered
	NotifyStateFailed  = "failed"  // retries exhausted or permanent failure, manually resendable
	NotifyStateSkipped = "skipped" // channel disabled, no longer sent
)

// Push modes.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode whitelists the push mode (same rationale as findings.status: no DB CHECK, easing
// future expansion).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel is one channel instance config. Config and Filter are kept as raw JSON;
// parsing is delegated to the notify package — the db layer doesn't understand their fields.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled is a pointer to distinguish "field not sent" from "explicitly false" —
	// the frontend switch control only submits changed fields.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled reports whether the channel is enabled; an unloaded Enabled (nil) is treated as enabled.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is one event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, enabled ones first, then by id.
// Sorting lives in SQL so the UI and dispatcher share one stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID fetches a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel creates or updates a channel.
//
// On update, only fields explicitly given by the caller (non-nil / non-empty) are overwritten, so the
// frontend can submit a drawer form of partial changes without echoing back config fields it never
// displayed — echoing them would cause "masked value overwrites the real key" accidents.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Deliberately **does not** do anything to 0 here: 0 is a valid config meaning "unlimited".
	//
	// It used to read `if c.RatePerMin <= 0 { c.RatePerMin = default }`, intended as "safe default
	// when unspecified", but that also swallowed "explicitly 0" — docs, UI hints, and takeTokens all
	// interpret 0 as unlimited, yet this quietly changed it to 20 (DingTalk/WeCom/Telegram) or 100
	// (Feishu), so operators thought they had lifted rate limiting while actually being capped at
	// 20/min with no hint.
	//
	// The difference between "unspecified" and "explicit 0" is known only to the caller (field absent
	// vs explicitly 0 in the request body), so defaults are filled in the server layer when the field
	// is absent; see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("rate limit value cannot be negative")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled toggles enable/disable.
//
// When disabling a channel, mark its not-yet-sent deliveries skipped: otherwise, after re-enabling,
// a batch of "backlogged during disable" old findings lands at once — stale and easily misread as new.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "channel disabled", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel deletes a channel. Its delivery history cascades away with the foreign key
// (once the channel config is gone, history is uninterpretable).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx **best-effort** writes one push event inside the caller's transaction.
//
// The only notification-related change on the vulnerability write path: a single INSERT, reading no tables, knowing no channels, running no filters. Transaction commit guarantees "vulnerability landed" and "push task exists" are atomic together —
// there is no window of successful commit with nothing enqueued and a permanently lost message.
//
// Two key designs, neither accidental:
//
//  1. **Why a SAVEPOINT**: in PostgreSQL, any statement error inside a transaction puts the whole transaction into aborted state, after which every statement (including COMMIT) fails. So "ignore this INSERT's error and let the caller still commit" is impossible in PG — unless a savepoint isolates the error to just that statement. Without a savepoint, the only option is rolling back the whole transaction.
//
//  2. **Why rolling back the whole transaction is wrong**: push is a convenience; the vulnerability record is the product. A notification-table issue (unmigrated old DB, transient disk fault) must not keep a critical vulnerability out of the DB. So the error is isolated, logged, and false returned, letting the vulnerability write commit normally — at the cost of losing that one push. Returning bool rather than error is deliberate: the caller must not treat it as an error that affects write success.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] failed to serialize push event finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] failed to create savepoint finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] failed to write push event finding=%d (finding record unaffected): %v", findingID, err)
		// Roll back to the savepoint, rescuing the transaction from aborted state.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] failed to roll back to savepoint finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release the savepoint to avoid accumulating useless savepoints in a long transaction.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is the standalone-transaction version of InsertNotificationEventTx, for call sites
// not in an existing transaction (e.g. a channel's "send test message", which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize notification event snapshot: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands not-yet-dispatched vulnerability events into delivery tasks across the currently enabled channels; returns the event count and new delivery count this round.
//
// The whole round runs in one transaction: events are claimed with FOR UPDATE SKIP LOCKED, so multiple processes running at once each claim different rows (the archive queue uses the same approach; see completeNextArchiveJob in db/task_archives.go).
//
// Filter matching is deliberately on the Go side rather than in SQL: a channel's filter is a JSONB with optional fields, and expressing six-combination matching in SQL would be hard to maintain, while the number of channels is "a handful configured by hand" — loading all of them and comparing in memory is faster and easier to test.
//
// Events matching no channel are still marked fanned_out — otherwise they stay in the pending set forever and get re-scanned every tick.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// The snapshot is written by ourselves, so it must parse in principle; a parse failure doesn't
		// block the delivery flow, but this event ends up skipped by every channel with filters because
		// its fields are all empty — better to drop one push than let one bad row stall the whole queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind takes the in-row value: the snapshot copy is for rendering and may have been written by an old version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Mark this round's events as dispatched. Events matching no channel are marked too (see func comment).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx fetches enabled channels inside a transaction. There are very few,
// so no paging or caching — caching would introduce the extra timing question of "when does a config change take effect".
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames resolves asset ids to short display names for push messages.
//
// Return order matches the input; length may be shorter than input (missing ids are skipped). Preserving
// input order keeps asset ordering stable across multiple deliveries of the same finding — otherwise a
// retry could change asset order and be misread as "the assets changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName picks the most recognizable identifier by asset type. Falls back to empty string; the caller decides how to present "assets whose name can't be got" — this function invents no placeholder, or "asset #42"-style noise would pollute push messages and readers would mistake it for a real domain.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify updates a finding's triage status and registers a status-change push event in the same transaction.
//
// Returns from=previous status; found=whether the finding exists; notified=whether the event registered.
//
// Three deliberate behaviors:
//   - No event is registered when the status didn't actually change. Repeated submissions of the same value from the frontend drawer, or idempotent replays from automation, must not produce push noise.
//   - A nonexistent finding returns found=false with no writes; the caller translates that into a 404.
//   - A failed event registration doesn't affect the status update (see the savepoint note in RecordNotificationEventTx), so when notified=false the status has already changed and the caller should not error out over it.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx updates finding status and registers a status-change push event within the **caller's
// transaction**.
//
// Lifted to a transaction-level function so every status-changing path shares one semantic — previously
// only patchFinding used the notifying version, while **when a retest concludes "fixed"** (the
// `UPDATE findings SET status=...` in finding_retests) it wrote directly to the DB, so channels configured
// with `on_status_change` received nothing for that flow: the status changed silently on screen, and ops
// only found out by opening the platform.
//
// Returns from=previous status, found=whether the finding exists, changed=whether status really changed,
// notified=whether the event registered (registration failure doesn't affect the update; see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// Don't register an event when status didn't really change: repeated submission of the same
		// value or idempotent replay must not generate push noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats is the overview counts at the top of the notifications page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // age in ms of the oldest pending delivery
}

// NotificationStatsSnapshot summarizes notification system health.
// BacklogAgeMS is the most direct "is push stuck" indicator — far more useful than the pending count,
// because a backlog of 3 can be 3 seconds or 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	//
	return &s, nil
}
