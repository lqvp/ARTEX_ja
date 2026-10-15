package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// This file covers the end-to-end behavior of push notifications: finding persisted → event → fan-out → real HTTP send.
//
// One security note: these tests **do not call the global Notifier.step()**; they only call stepRealtime/stepDigest
// on channels they created themselves. The reason is that step() walks every enabled channel in the database —
// running the tests against a dev database with real DingTalk/WeCom bots configured would make the global step
// actually push findings produced during the tests into those groups. Calling per channel strictly limits the blast radius to the fake receivers the test itself creates.
//
// Cleanup: at the end of each test, delete the events it produced (deliveries cascade-delete) along with its channels, leaving no backlog in real channels.
//
// Assertion stance: stepRealtime/stepDigest return nothing and log internally, so what is asserted here is
// **observable external behavior** (what the fake receiver received, what state the delivery row ended in), not the
// function's return value — this is closer to the real call path than stubbing return values.

// notifyFixture is the shared fixture for the tests in this file.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// Self-created task/exploration: tests record findings here, isolated from other tests' data.
	taskID int64
	expID  int64
	// Events created after cleanupMark are deleted together during cleanup.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// Every fake receiver in this file runs on 127.0.0.1, while delivery rejects loopback addresses by default
	// (to stop SSRF from reaching same-host services and cloud metadata). The test flips this switch explicitly;
	// the "denied by default" guard itself is covered by the notify package's ssrf_test.go.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// Create a task of our own: the task built by the shared trafficEvidenceServer fixture exposes no exploration id,
	// yet recording a finding requires one.
	task, err := s.m.CreateTask("notification push test", "push behavior verification", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// Make the fixture self-contained: mark every event that predated the fixture as already fanned out.
	//
	// Why this is necessary: FanOutPendingEvents is **global** — it expands every undispatched event in the
	// database onto every matching channel. The shared trafficEvidenceServer fixture records a finding itself
	// (exactly the initial finding it returns), and other tests may leave residue behind. Without isolation, those
	// stray events get fanned out onto this test's channel, making assertions like "there should be N deliveries"
	// pass and fail at random — and the outcome depends on test execution order, which is harder to debug than an outright failure.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("failed to clean up notification events: %v", err)
		}
	})
	// The master switch must be on (other tests may have turned it off).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record persists a finding through the real evidence-write path and returns the finding id.
// That path registers the notification event in the **same transaction** — the hook this feature hangs on.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " summary",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("failed to record finding: %v", err)
	}
	return out.FindingID
}

// channel reads back the channel config (so stepX can be called per channel).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("failed to read channel: %v", err)
	}
	return ch
}

// deliver fans out pending events and runs one delivery round for the given channel only.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("failed to fan out pending events: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel creates a channel through the HTTP API, also covering that API's own validation paths.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("failed to create channel %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("unexpected response from channel creation: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook is a fake receiver that records the request bodies it receives.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("fake receiver received only %d requests; cannot fetch request #%d", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("fake receiver received no requests")
	}
	return f.body(t, f.count()-1)
}

// markdownText extracts the body text from a request body, tolerating vendors' differing field names:
// DingTalk markdown uses `text`, ActionCard uses `text`, WeCom markdown uses `content`.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("request body has no recognizable message text: %v", body)
	return ""
}

// agePendingBatch ages the channel's pending deliveries, for testing digest batch expiry.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "Realtime push",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL injection", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("expected 1 message to be sent, got %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL injection", "🟠 High", "Summary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("message body is missing %q:\n%s", want, text)
		}
	}
	// The delivery should end up as sent.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries are still not marked sent after delivery", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "masking case",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("failed to list channels %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API response leaks credentials: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("newly created channel did not appear in the list")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("credential fields should be masked values: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API should tell the frontend which fields are credentials")
	}

	// PATCH changes only the name and echoes back the masked credentials: the real credentials must survive untouched.
	body, _ := json.Marshal(map[string]any{
		"name":   "renamed channel",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("update failed %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("echoing the masked value back overwrote the real credential: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("echoing the masked value back overwrote the secret: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "renamed channel" {
		t.Fatal("name was not updated")
	}

	// Explicitly clearing secret must take effect (unlike "send the masked value back = keep unchanged").
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("clearing the secret failed %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("an empty string should clear the secret")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"invalid kind", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "Invalid channel kind"},
		{"missing name", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "Channel name is required"},
		{"missing webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"invalid webhook protocol", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "invalid"},
		{"invalid mode", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "push mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("expected 400, got %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("error message should mention %q, got %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("deleting a nonexistent channel should return 404, got %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "critical only",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "Low severity issue", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a finding below the threshold should not produce deliveries, got %d", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("a filtered finding should not send a message")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "digest push",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("Digest Vuln %d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// Not yet due: nothing is sent.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("digest batch was sent before it was due")
	}

	// After aging the batch: the three findings combine into one message.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("three findings should batch into one message, but %d were sent", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "min: +3 vulnerabilities") { // intro renders "**Last %d min: +%d vulnerabilities**" (notify/markdown.go markdownBatchIntro); window = notify_digest_interval_min, default 30; the "continue in the next message" clause only appears when items are dropped from this message
		t.Fatalf("digest message missing count/time-window copy:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("Digest Vuln %d", i)) {
			t.Fatalf("digest message missing item %d:\n%s", i, text)
		}
	}
	// Deliveries in the same batch should share a batch_id.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("the three deliveries should share one batch_id, got distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "disabled channel",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "Finding while disabled", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a disabled channel should not produce deliveries, got %d", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "status change subscription",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "Status change case", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// There should be two: the fixed one is the status change; the finding_created one may also go out in the same round.
	// The status-change delivery is in fact created later, but this does not depend on order — scan them all.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "Status change") {
			found = true
		}
	}
	if !found {
		t.Fatalf("received no message containing \"Status change\" (%d messages total)", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "no status change subscription",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "Unsubscribed change", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("failed to change status %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a channel not subscribed to status changes should not receive status-change deliveries, got %d", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "test send",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("test send failed %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("the fake receiver should receive 1 test message, got %d", hook.count())
	}
	// The test message must be obviously a test and must not be mistaken for a real vulnerability.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "Test message") {
		t.Fatalf("test message should be marked as a test: %s", text)
	}
	// When the config is broken, the channel's raw error must be reported verbatim to the user.
	badID := f.createChannel(t, map[string]any{
		"name":   "broken address",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("a failed delivery should return 502, got %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// Point at an address that is bound to fail, to manufacture a failed delivery.
	chID := f.createChannel(t, map[string]any{
		"name":   "failure retry",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "Push that will fail", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// Keep delivering until the retry budget is exhausted.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("after the retry budget is exhausted the state should be failed, got %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("failed to query history %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("expected to find 1 failed delivery, got total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("history should carry the failure reason; otherwise the user cannot troubleshoot")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("attempt count should be recorded, got %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "Push that will fail" {
		t.Fatalf("history should carry the finding title, got %q", hist.Deliveries[0].Title)
	}

	// Manual retry: it should return to pending with the counter reset.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("manual retry failed %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("after retry the state should be pending with attempts=0, got %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta request failed: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta should list all %d channels, got %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("channel %s did not report its credential fields", k.Kind)
		}
	}

	// Round-trip the three global settings. The trailing slash must be normalized away, otherwise the deep link would come out as "//function/...".
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("failed to write settings %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("public base URL was not normalized: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("digest interval did not take effect: %v", payload["notify_digest_interval_min"])
	}

	// Invalid values must be rejected.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s should return 400, got %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL covers deep-link assembly: when public_base_url is configured,
// a single message must use an ActionCard with a button, and the link must point at the finding detail page.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "deep link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "Finding with deep link", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("with a deep link an ActionCard should be used, got msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("wrong deep link\nwant %s\ngot %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL is the negative case: with no external address configured, no broken link
// should be produced (e.g. pointing at localhost or a relative path); it should fall back to plain markdown.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "no deep link",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "Finding without deep link", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("without an external address the message should be markdown, got %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "View details") {
		t.Fatalf("no detail link should appear when no external address is configured:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder is the end-to-end evidence for the "silent loss" fix.
//
// Digest messages are bounded by the channel length cap (WeCom: 4096 bytes); when a batch does not fit, it must be
// split **by whole items**: those fitting in this message are marked delivered, the rest return to the queue for the
// next one. The old implementation marked the entire batch as succeeded — the truncated items were neither in the
// message nor in the failure list, and the delivery history still showed success, so the findings simply vanished.
//
// Four things are asserted: (1) only the count actually fitting is marked, (2) the rest are still pending,
// (3) deferred items **consumed no retry attempts**, (4) another round sends the remainder (no deadlock).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// Use WeCom: its markdown cap is 4096 bytes, the tightest of the six channels.
	chID := f.createChannel(t, map[string]any{
		"name":   "segmented digest",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// Use longer titles so the 60 items far exceed 4096 bytes and segmentation is inevitable.
	longName := strings.Repeat("Very long vulnerability name", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("only one message should be sent, got %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("some items should have been marked delivered")
	}
	if pending == 0 {
		t.Fatalf("%d items cannot all fit into 4096 bytes in one batch; some should remain pending; sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("item count mismatch: sent=%d pending=%d total=%d (neither delivered nor pending = lost)", sent, pending, total)
	}
	// The message body must honestly state how many items were left out of this message.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "remaining") {
		t.Fatalf("message should state that items were left out of this message:\n%.400s", text)
	}

	// Deferred items must not consume the retry budget: attempts was optimistically +1 when claimed, so it must be decremented on deferral.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("deferred items must not consume retry attempts (otherwise they would be judged failed after a few rounds), got attempts=%d", maxAttempts)
	}

	// Keep running until it converges. What is asserted is **everything eventually delivered** and that it really
	// took multiple rounds — this is stronger than "finished in round two": it proves segmentation neither deadlocks nor drops the remainder.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("segmented delivery does not converge: after %d rounds %d items are still unresolved", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("round %d made no progress; the remaining %d items would be stuck forever", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("a 4096-byte message cannot hold %d long-titled findings; delivery should take multiple rounds but only used %d", total, rounds)
	}
	// Every round after the first should be a pure continuation, with no items rejected by the channel.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("the fake receiver always returns success; there should be no failed items, got %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget is a drift guard.
//
// The retry budget (db.MaxNotifyAttempts) and the backoff schedule (notifyBackoff) live in two different packages:
// the former is the state machine's policy, the latter the engine's execution cadence. If only one is changed — say
// the budget is raised to 5 but no backoff step is added — the code will not complain; retries 4 and 5 simply reuse
// the last interval, showing up as "the retry pace mysteriously slowed down", which is hard to trace back here.
// Asserting the two lengths match exposes such drift in CI.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("backoff table length (%d) does not match the max attempt count (%d) — changing one requires changing the other",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// Backoff intervals must be monotonically non-decreasing, otherwise retries get more aggressive over time and worsen rate limiting.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("backoff intervals must be monotonically non-decreasing: step %d %v < step %d %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget locks down the "take tokens first, then claim" order.
// If it were reversed (claim first, then discard), a delivery blocked by rate limiting would already have been
// counted once in attempts, and the budget would be drained by pure waiting until it finally lands in failed.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// Only the token bucket itself is under test; no Server is needed (and none should be built for it).
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// One per minute: with a full bucket, at most one.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("with a full bucket a rate of 1/min should take 1 token, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("once tokens are exhausted it should return 0 immediately, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("halfway through the cycle a full token should not be refilled yet, got %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("after one full cycle 1 token should be refilled, got %d", got)
	}
	// Unthrottled channels use a finite cap so one round cannot be held up by unbounded backlog.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("unthrottled should return the per-round cap %d, got %d", notifyUnlimitedBurstPerTick, got)
	}
	// Token buckets are independent across channels.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("channel 1's bucket should still be empty, got %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens locks down the "take only want tokens" semantics.
//
// The old implementation drained the entire bucket and let the caller truncate, so a rate=100/min channel filled
// its bucket, used only 5 items in a round and threw the other 95 tokens away; the same deduction applied even
// when the channel had no pending deliveries that round. The comment's claim that "a backlog can burst rate_per_min items at once" could never happen.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// The bucket starts full (100); this round asks for only 5.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("with want=5 exactly 5 tokens should be taken, got %d", got)
	}
	// Key assertion: the remaining 95 must still be in the bucket, not drained and discarded.
	// Time is not advanced, so any tokens taken can only come from the existing stock, not from refill.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("remaining tokens should still be available (expected 95), got %d — the bucket was drained entirely", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("the bucket is empty; should return 0, got %d", got)
	}
	// want<=0 must not deduct any tokens (an empty round is free of charge).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 should return 0, got %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("the want=0 call must not consume tokens; 20 should still be available, got %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget pins down the two dimensions of digest mode.
//
// Once digest batch size is tied to the per-round request budget, a rate_per_min=20 channel can refill only 1 token
// in each 3-second tick, so every digest message carries just 1 finding — functionally no digest at all, while the
// message header still says "**Last 30 min: +1 vulnerabilities**". This regression raises no error, and the existing
// end-to-end tests cannot see it either (they pass a big enough limit to stepDigest manually, bypassing the budget
// calculation in step), so the decision itself is asserted directly here.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// One batch = one message = one request = one token. The token is counted in messages, not findings.
	if tokens != 1 {
		t.Fatalf("one digest batch sends one message; it should consume exactly 1 token, got %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("digest batch size should be the memory cap db.MaxDigestBatchSize=%d, got %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// Key relation: batch size must be far larger than the per-round request budget. Once they are the same order
	// of magnitude, it means "how many messages to send" and "how many findings fit in one batch" have been conflated into a single number.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("digest batch size %d should not be constrained by the per-round request budget %d — "+
			"the request budget is 'how many requests to send' as back-derived from the lease; 'how many findings fit in one batch' is a different dimension",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease is another drift guard.
//
// The per-round delivery cap for one channel (notifyMaxSendsPerChannelPerTick) is derived from the lease duration:
// the worst-case time to deliver serially in one round must be < the lease, otherwise the lease expires before the
// last few go out and, in a multi-instance deployment, peers reclaim them and send duplicates. These three constants
// live in different places, so changing any one can break the relation without any error — hence pinned down here.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("worst-case per-round time for one channel %v should not reach or exceed the lease %v"+
			" (notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v) — "+
			"changing any one of these three constants requires re-checking the other two",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}

