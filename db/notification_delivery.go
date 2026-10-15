package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file covers claiming and state transitions of delivery tasks.
//
// Claiming uses a "lease" rather than a long transaction: set the row to sending and push next_attempt_at into the future as the lease expiry, then commit the transaction before doing the network delivery. That way no DB lock is held during delivery — a network request may take seconds (client timeout 15s), and holding the row lock would stall other writers on the same DB.
//
// The cost: if the process crashes mid-delivery the row stays sending. This is **self-healing**: once the lease expires next_attempt_at falls into the past, and the next claim cycle scoops the same row up again (see the state IN ('pending','sending') condition in the claim query). The retry counter is already +1 at claim time, so a crash never causes infinite retries — after MaxNotifyAttempts attempts the row lands in failed for manual handling.

// MaxNotifyAttempts is the max attempts for one delivery (including the first).
// Defined here rather than in the delivery engine: it is the state machine's own policy; the engine only executes.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize is the max deliveries merged into a single digest batch at once.
//
// The reason it exists is resources: within one digest cycle a scan may surface tens of thousands of
// findings (entirely possible — one full scan can do it); without an upper bound claiming would read
// every row into memory and render one ultra-long message, then have most of it truncated by the
// channel's length limit — wasting memory and **silently losing** the findings that got cut. With an
// upper bound, the remainder stays in the DB as the next batch and goes out naturally the next cycle;
// nothing is lost.
//
// The basis for 500: it is the order of magnitude that still leaves readable content inside WeCom's
// 4096-byte limit after rendering; going larger only pushes truncation further back.
const MaxDigestBatchSize = 500

// NotificationDelivery is one delivery task, including the channel config and event snapshot needed for rendering.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// Render context loaded jointly, not serialized (assembled into a DTO by the server layer).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind carried out of the event, so the history list can jump straight to the finding detail.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind are redundant display fields for the list, avoiding a second query on the frontend.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery is the uniform read shape of a delivery row: delivery + event snapshot + channel config.
// Rendering a message needs all three; separate queries would mean three round trips.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery describes one claim: first sel selects candidates and locks them, then they are set to sending and
// the lease extended. The lease slot inside sel is placeholdered with $n and supplied by the caller.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries claims a batch of due realtime deliveries for one channel, at most limit rows.
//
// Deliberately claimed **per channel** rather than "claim a global batch and then cherry-pick": the rate-
// limit gate is maintained per channel in the delivery engine, and only by first knowing how many this
// channel can still send this round and then claiming that many rows does rate limiting avoid consuming
// retry attempts. Done the other way (claim then discard), rows blocked by rate limiting have already been
// counted once in attempts; the 3-attempt budget would be burned by pure waiting, ending up in failed.
//
// The conditions include "sending with expired lease" — that is the crash-recovery landing spot. Lease must
// be significantly greater than the worst-case delivery cost (channel HTTP client timeout 15s), or the same
// row gets delivered by two dispatchers at once. Disabled channels are also excluded here: disable already
// marks outstanding deliveries skipped, and this is a second line of defence against races between disable
// and claim.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue reports whether a channel has accumulated a due batch: pending deliveries exist and the
// **oldest** one's age has reached the digest cycle.
//
// The decision is based on the oldest delivery's age rather than wall-clock time, so a freshly created
// channel won't immediately emit a one_ITEM "digest" by aligning to the hour, and a long-backlogged batch
// won't wait another idle round.
//
// Split from ClaimDigestBatch because the semantics differ: this answers "should it send"; claiming
// takes away **all** of the channel's pending deliveries (including those not yet of full age) — otherwise
// one cycle would be split into several messages, defeating the purpose of digesting.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch claims a channel's currently due pending deliveries as one digest batch,
// at most MaxDigestBatchSize rows.
//
// All deliveries in a batch share batch_id, using the smallest id in the set as the batch number
// (stable, readable, no extra sequence). On retry, COALESCE keeps the original batch number, so "these
// N rows were sent together" still holds across multiple retries.
//
// Taking the first N rows in id order rather than at random: the earliest-produced deliveries go out
// first, so under backlog there is no starvation where "new findings ship first, old ones never".
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit is an **in-memory upper bound**: the caller passes MaxDigestBatchSize; clamp again here
	// to guard against a caller passing a larger value.
	//
	// Deliberately not accepting "rate-limit allowance" as batch size: the rate-limit unit is messages —
	// one batch sends one message and consumes one token, deducted by takeTokens in the server layer —
	// a different dimension from "how many findings fit in a batch". Earlier, to make rate_per_min
	// effective for digest, the per-round request budget was passed in as batch size, and a channel with
	// rate=20/min ended up batches of just 1 finding, degrading digest into realtime pushes with a
	// digest header. To change rate limiting edit takeTokens's want, not this.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries runs "select + set to sending with extended lease + read full rows", all in one transaction.
// postClaim is an optional extra step (digest batches use it to write batch_id).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// Set to sending and push next_attempt_at into the future: that future moment is the lease expiry,
	// so "lease not yet expired" and "retry time not yet reached" share the same predicate — no new column needed.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent marks a batch of deliveries as delivered.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries moves a batch of deliveries back to pending and pushes retry time forward.
//
// Moving back to pending rather than introducing a new intermediate state keeps "how many chances remain"
// expressed in one place (MaxNotifyAttempts), avoiding state-machine branches expanding with retry policy.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries moves a batch of deliveries back to pending, immediately re-claimable, and **reverts
// the attempt counted at claim time**.
//
// Single purpose: when a digest message is sent in segments bounded by the channel length limit, entries that didn't fit in this segment are left for the next batch. That is not a failure, so it must not consume retry budget — attempts was optimistically +1 at claim time, and must be decremented here.
// Otherwise a 500-row backlog split into 20-row segments becomes 25 segments, and tail entries get ruled failed by MaxNotifyAttempts by the 3rd segment despite never having errored.
//
// GREATEST(...,0) guards the case "someone manually resent, zeroing attempts, then it reaches here anyway",
// keeping the count from going negative.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries marks a batch of deliveries as finally failed, awaiting manual resend from the history.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// Placeholders start at $3: $1 is state, $2 is last_error.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery manually resends one delivery: reset to pending, zero the retry counter,
// due immediately. Zeroing the counter is deliberate — clicking "resend" means the causes of earlier
// failures have been addressed, so limiting it with the old count makes no sense.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delivery %d does not exist or its current state does not allow resending", id)
	}
	return nil
}

// NotificationDeliveryFilter is the query condition for delivery history.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries returns delivery history, newest first, paginated.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError truncates an error message to a column-safe length. Channel response bodies can be
// long (especially when a generic webhook hits a self-hosted service); without truncation the history
// list payload bloats.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// Walk back to a character boundary so we don't leave half a UTF-8 byte that renders as garbled text.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders builds the $n placeholder string and matching args starting at start, for use in IN (...).
// e.g. start=3, ids=[7,8] -> "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
