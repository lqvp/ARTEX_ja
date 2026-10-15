package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (stored in the settings key-value table; no extra table needed).
const (
	// settingNotifyEnabled is the master switch for push notifications. It defaults to on: it is for
	// one-click shutdown during maintenance, not a precondition of the feature — the real precondition is whether a channel is configured.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the externally reachable base URL used to build vulnerability
	// detail backlinks (e.g. https://artex.example.com). Left empty, messages carry no backlink button.
	// The project has no reusable external-URL setting, so one is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest mode interval (minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. Three seconds is the ceiling on its
	// freshness and the main delay from "vulnerability persisted" to "message delivered to the IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be well above the worst-case
	// time of a single delivery (the notify package's HTTP client times out after 15 seconds), otherwise
	// the same row can be claimed by two dispatchers at the same time.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick caps how many events are dispatched per tick, so that enabling a channel
	// for the first time does not expand the entire historical backlog into delivery tasks at once.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-tick delivery cap for channels with no rate limit.
	// It exists to keep "one channel configured as unlimited plus a scan turning up thousands of
	// findings" from stretching a single loop iteration into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is how many deliveries a single channel may send per tick.
	//
	// This cap is derived backwards from the **lease duration**: claiming stamps the row with a lease
	// (notifyLease = 3 minutes), and if one tick serially delivers so many items that the worst case
	// exceeds the lease, the lease expires before the last few go out. Inside a single process it does
	// not matter (Run runs serially in one goroutine and ticks never re-enter), but with **two processes
	// on one database** the peer reclaims the expired rows and resends them, double-increments attempts, and marks them failed while the original process is still delivering.
	//
	// Value: 3-minute lease / 30-second per-send timeout = 6 fills the lease exactly with zero margin,
	// so it cannot be used; 5 gives a 150-second worst case with 30 seconds of margin. This relationship
	// is pinned by TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout, or
	// this value will fail that assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the previous
	// constant; the two multiplied must not exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by the number of attempts made.
// Three chances (first included) correspond to db.MaxNotifyAttempts; both must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability notifications.
//
// It runs as a standalone goroutine alongside the Scheduler (see server.New). It deliberately does
// not reuse the Scheduler's tick: the freshness requirement (3 seconds) differs from the triggers'
// business cadence, and their failures are independent — a stuck notification must not affect agent triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu guards buckets. There are few channels and little contention, so one mutex is enough;
	// finer-grained structures are not worth introducing for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for a single channel.
//
// A token bucket rather than a sliding window that "counts per minute then resets", because the
// latter has awful boundary behavior: send 20 at the end of the window and 20 more an instant later,
// which the platform sees as 40 in one second and rate-limits; a token bucket refills at a steady rate and naturally avoids such bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx is done. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one tick: first dispatch new events, then deliver the tasks that are due.
//
// Any failure only logs and never breaks the loop — a notification outage must never escalate into a
// process-level problem. Every tick is independent and the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to list channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket counts **messages** (equivalent to HTTP requests), not vulnerabilities.
		// In realtime mode the two coincide (one vulnerability, one message); in digest mode a whole
		// batch of vulnerabilities collapses into one message, so it costs a single token.
		//
		// Both modes ask the token bucket first and then claim against the quota — the order cannot
		// be reversed, or a rate-limited delivery would already have burned a retry attempt.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the token cost and the batch size cap for a digest channel this tick.
//
// The two return values are **two different quantities**, which is exactly why this is its own function:
//
//   - tokens is the number of messages. A batch of vulnerabilities becomes one message sent as a single HTTP request, so it is always 1.
//     rate_per_min therefore still applies to digest (at most that many digest messages per minute).
//   - claimLimit is how many vulnerabilities a batch may hold. It is bounded only by the memory cap, independent of the request budget.
//
// To make rate_per_min apply to digest, an earlier version passed the per-tick request budget
// (notifyMaxSendsPerChannelPerTick, derived backwards from the lease) directly down as the batch size.
// The result: a channel with rate_per_min=20 only refilled 1 token within a 3-second tick, so each digest
// message held a single vulnerability — digest degenerated into "a realtime push with digest wording", and readers received a string of
// 「Last 30 min: +1 vulnerabilities」 while db.MaxDigestBatchSize stayed permanently unreachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases all pass a large enough limit to
// stepDigest manually, bypassing the quota calculation in step), so the decision is collected here and pinned by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget directly.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers a channel's realtime tasks: one vulnerability, one message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Rendering failed because of local data; retrying will not improve it.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message and sends it when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to check digest batch due channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel kind %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries with a broken snapshot that never made it into the message must be explicitly failed.
	// Otherwise they stay outside included — in neither the message nor the failure list — and when the
	// send succeeds, the later batch marking skips them, leaving them stuck in sending until the lease expires and they are reclaimed repeatedly.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed; this vulnerability cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark broken-snapshot deliveries failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries whose snapshots could not be parsed channel=%d", len(skipped), ch.ID)
	}
	// Hand only the ones that made it into the message to send: included[i] corresponds exactly to
	// msg.Items[i], and send relies on that mapping to apply "the channel reported it fit the first K" to the right delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions state according to the result.
//
// One batch of deliveries (possibly dozens in digest mode) shares a single send result: either all
// delivered, or the whole batch is retried. No per-item retry — a digest message is one message, and resending part of it would corrupt the batch semantics.
//
// The only exception is **segmentation caused by the channel's length limit**: if the channel reports only
// the first K fit, everything from K+1 onward must wait for the next batch instead of being marked
// sent along with the rest. Otherwise those truncated vulnerabilities are in neither the message nor the failure list — they vanish entirely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Cap a single send so one stuck channel cannot hold up the remaining channels this tick.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel cannot report more deliveries than were sent; if it does, the rendering layer
			// miscalculated. Treat everything as delivered and log it — better than corrupting records.
			log.Printf("[notify] channel reported %d delivered exceeding %d deliveries channel=%s; treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark deliveries sent channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message has reached the channel length limit: the rest go straight back to the queue and are sent by the next tick.
			// Use DeferDeliveries instead of RescheduleDeliveries — this is not a failure and must
			// not consume the retry budget (claiming already optimistically added 1, which is subtracted back there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("message reached the channel length limit; only the first %d were delivered, the rest wait for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to requeue segmented continuation channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither errored nor reported how many were delivered. Treat it as a failure (goes
		// through backoff), so this delivery is not claimed over and over yet never marked done.
		err = fmt.Errorf("channel did not report delivered count (delivered=%d)", delivered)
	}

	// Failure handling is decided **per delivery**, not by the highest attempt count in the batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` condemning the whole batch, but attempts
	// differ across a batch: an old delivery that already retried twice (attempts=2) drags the brand-new
	// ones in the same batch (attempts=1) into failed — a fresh vulnerability is lost for good without
	// ever using a single retry, exactly opposite to the intent of "never let old rows sink new ones".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] failed to mark failure state channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d attempts: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark failure state channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Re-group by delay: there are only 3 backoff tiers, so the number of groups is naturally small,
	// avoiding one UPDATE per delivery (which would be 500 round trips for a 500-item batch).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d retries-exhausted=%d pending-retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns the ones in all that are not in keep (compared by pointer identity).
// Used to find deliveries that "did not make it into the message" — they must be handled explicitly, not left in a grey zone.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// ok=false means the kind is unregistered; the delivery should fail outright instead of retrying forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On a config parse failure, hand over an empty map: the channel's own Validate reports which
		// field is missing, and that error guides the user better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders the message for a single vulnerability.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders the digest message. Snapshots are parsed one by one — a single broken one only
// skips that item, without sinking the whole digest batch.
//
// The return value included corresponds **strictly one-to-one** with msg.Items (i'th delivery ↔ i'th item).
// This mapping is a hard requirement: the caller decides that the first K deliveries are sent based on
// "the channel reported it fit the first K". If a broken snapshot is skipped here without removing the
// skipped delivery from included, the indices shift — broken items that should fail get marked sent, while
// good items are wrongly judged undelivered. The broken ones are explicitly failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// Broken snapshots stay out of the message and out of included — their handling belongs
			// to the caller (explicitly failed, not smuggled in among the "sent" ones).
			log.Printf("[notify] skipped unparsable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("none of the %d deliveries in the digest batch could be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a push item, also resolving asset names and the detail backlink.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Failing to resolve asset names should not block the push: a missing name is far lighter than
		// a missing notification — the message just loses an asset line.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail page route is web/src/app/(main)/function/findings/detail/page.tsx;
		// it reads the vulnerability id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel's bucket and returns how many it actually got.
//
// One token = one message (one HTTP request). In realtime mode the caller passes however many it needs;
// in digest mode a whole batch of vulnerabilities goes out as one message, so it passes 1.
//
// Bucket capacity is the channel's per-minute limit, refilled at a constant rate. ratePerMin<=0 means
// no rate limit: a finite but large value is returned so one loop iteration cannot be dragged down by an unbounded backlog.
//
// The want cap is essential: without it the bucket can only be drained completely, while the caller has
// its own per-tick cap — tokens taken beyond that are never used and vanish before the next refill, so the
// saved burst capacity is unreachable and even "no deliveries due this tick" still gets charged.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill on elapsed real time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: token counts are float accumulations, and filling in two
	// steps can make 0.5 + 0.5 come out as 0.9999999999, which int() would truncate to 0 — a bucket
	// that is mathematically full would yield no tokens. 1e-9 is far below one token, so a real shortfall is still caught.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for backlinks, with the trailing slash trimmed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default when it is invalid or unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event a delivery points to.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("delivery %d has an empty event snapshot", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event kind follows the event row; the copy in the snapshot may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
