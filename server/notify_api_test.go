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
	task, err := s.m.CreateTask("通知推送测试", "推送行为验证", nil, 0, 0)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
			t.Logf("清理通知事件失败: %v", err)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("记录漏洞失败: %v", err)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	return out.FindingID
}

// channel reads back the channel config (so stepX can be called per channel).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("读渠道失败: %v", err)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	return ch
}

// deliver fans out pending events and runs one delivery round for the given channel only.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("分派失败: %v", err)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel creates a channel through the HTTP API, also covering that API's own validation paths.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("建渠道失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("建渠道返回异常: %s (%v)", r.Body, err)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("假接收端只收到 %d 条请求，取不到第 %d 条", len(f.bodies), i)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("假接收端没有收到任何请求")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
	t.Fatalf("请求体里没有可识别的正文: %v", body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		"name":   "实时推送",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL注入", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("应发出 1 条消息，实际 %d", hook.count())  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQL注入", "高危", "摘要"} {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		if !strings.Contains(text, want) {
			t.Fatalf("消息正文缺少 %q:\n%s", want, text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
	}
	// The delivery should end up as sent.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("投递后仍有 %d 条未标记 sent", pending)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "掩码用例",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("列渠道失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("接口回显泄露了凭据: %s", r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatal("新建的渠道未出现在列表里")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("凭据字段应为掩码值: %v", mine.Config)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("接口应告知前端哪些字段是凭据")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// PATCH changes only the name and echoes back the masked credentials: the real credentials must survive untouched.
	body, _ := json.Marshal(map[string]any{
		"name":   "改名后",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("掩码回传把真凭据覆盖了: %v", cfg["webhook"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("掩码回传把 secret 覆盖了: %v", cfg["secret"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if f.channel(t, chID).Name != "改名后" {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		t.Fatal("名字未更新")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// Explicitly clearing secret must take effect (unlike "send the masked value back = keep unchanged").
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("清空 secret 失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空串应清空 secret")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		{"类型非法", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "Invalid channel kind"},  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		{"缺名称", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "Channel name is required"},  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		{"缺 webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		{"webhook 协议非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "invalid"},  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		{"模式非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "push mode"},  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("应返回 400，得到 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("错误信息应提到 %q，得到 %s", tc.wantSub, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("删除不存在的渠道应 404，得到 %d", r.Code)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "仅严重",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低危问题", "low")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("低于阈值的漏洞不该产生投递，得到 %d 条", n)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("被过滤的漏洞不应发出消息")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "汇总推送",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("汇总漏洞%d", i+1), "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// Not yet due: nothing is sent.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("汇总批次未到期就发了")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// After aging the batch: the three findings combine into one message.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("三条应汇总成一条消息，实际发了 %d 条", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "continue in the next message") || !strings.Contains(text, "3 vulnerabilities") {
		t.Fatalf("汇总消息缺少条数/时间窗文案:\n%s", text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("汇总漏洞%d", i)) {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			t.Fatalf("汇总消息缺少第 %d 条:\n%s", i, text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
	}
	// Deliveries in the same batch should share a batch_id.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("三条投递应共享一个 batch_id，得到 distinct=%d total=%d", distinct, total)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停用渠道",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停用期间的漏洞", "critical")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停用渠道不该产生投递，得到 %d 条", n)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状态变更订阅",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状态变更用例", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("没有收到含「状态变更 → 已修复」的消息（共 %d 条）", hook.count())  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "不订阅状态变更",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "不订阅变更", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("未订阅状态变更的渠道不该收到状态变更投递，得到 %d 条", n)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "测试发送",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("测试发送失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if hook.count() != 1 {
		t.Fatalf("假接收端应收到 1 条测试消息，得到 %d", hook.count())  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// The test message must be obviously a test and must not be mistaken for a real vulnerability.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "测试") {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		t.Fatalf("测试消息应标明是测试: %s", text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// When the config is broken, the channel's raw error must be reported verbatim to the user.
	badID := f.createChannel(t, map[string]any{
		"name":   "坏地址",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("投递失败应回 502，得到 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// Point at an address that is bound to fail, to manufacture a failed delivery.
	chID := f.createChannel(t, map[string]any{
		"name":   "失败重试",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "会失败的推送", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("重试耗尽后应为 failed，得到 %s", state)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("查历史失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("应查到 1 条失败投递，得到 total=%d len=%d", hist.Total, len(hist.Deliveries))  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("历史里应带上失败原因，否则用户无法排查")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("尝试次数应被记录，得到 %d", hist.Deliveries[0].Attempts)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if hist.Deliveries[0].Title != "会失败的推送" {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		t.Fatalf("历史应带出漏洞标题，得到 %q", hist.Deliveries[0].Title)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// Manual retry: it should return to pending with the counter reset.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("重发失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("重发后应为 pending 且 attempts=0，得到 %s/%d", state, attempts)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 失败: %s", r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("meta 应列出全部 %d 个渠道，得到 %d", len(notify.Kinds()), len(meta.Kinds))  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("渠道 %s 未上报凭据字段", k.Kind)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
	}

	// Round-trip the three global settings. The trailing slash must be normalized away, otherwise the deep link would come out as "//function/...".
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("写设置失败 %d: %s", r.Code, r.Body)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("回链地址未规范化: %v", payload["notify_public_base_url"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("汇总周期未生效: %v", payload["notify_digest_interval_min"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// Invalid values must be rejected.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s 应返回 400，得到 %d", body, r.Code)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL covers deep-link assembly: when public_base_url is configured,
// a single message must use an ActionCard with a button, and the link must point at the finding detail page.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "回链",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "带回链的漏洞", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("有回链时应用 ActionCard，得到 msgtype=%v", body["msgtype"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("回链不对\n期望 %s\n得到 %v", want, card["singleURL"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL is the negative case: with no external address configured, no broken link
// should be produced (e.g. pointing at localhost or a relative path); it should fall back to plain markdown.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "无回链",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "无回链的漏洞", "high")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("未配外部地址时应发 markdown，得到 %v", body["msgtype"])  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if text := markdownText(t, body); strings.Contains(text, "查看详情") {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		t.Fatalf("未配外部地址时不该出现详情链接:\n%s", text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		"name":   "分段汇总",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// Use longer titles so the 60 items far exceed 4096 bytes and segmentation is inevitable.
	longName := strings.Repeat("超长漏洞名称", 6)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("应只发出一条消息，得到 %d", hook.count())  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatal("应有条目被标记为已送达")  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if pending == 0 {
		t.Fatalf("一批 %d 条不可能全装进 4096 字节，应有剩余待发；sent=%d", total, sent)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if sent+pending != total {
		t.Fatalf("条目数对不上：sent=%d pending=%d total=%d（既没送达也没待发=丢失）", sent, pending, total)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// The message body must honestly state how many items were left out of this message.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "其余") {  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		t.Fatalf("消息应说明还有条目未包含在本条:\n%.400s", text)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}

	// Deferred items must not consume the retry budget: attempts was optimistically +1 when claimed, so it must be decremented on deferral.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("被推迟的条目不该消耗重试次数（否则几条之后就会被判失败），得到 attempts=%d", maxAttempts)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
			t.Fatalf("分段投递不收敛：跑了 %d 轮仍有 %d 条悬而未决", rounds, undelivered)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("第 %d 轮没有任何进展，剩余 %d 条会永久卡住", rounds, undelivered)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
		}
	}
	if rounds < 2 {
		t.Fatalf("一条 4096 字节的消息装不下 %d 条长标题漏洞，应分多轮发出，实际只用了 %d 轮", total, rounds)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// Every round after the first should be a pure continuation, with no items rejected by the channel.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("假接收端始终返回成功，不该有失败条目，得到 %d", failed)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("退避档位数(%d)与最大尝试次数(%d)不一致——改一个必须同时改另一个",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// Backoff intervals must be monotonically non-decreasing, otherwise retries get more aggressive over time and worsen rate limiting.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("退避间隔必须单调不减：第 %d 档 %v < 第 %d 档 %v",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("满桶时每分钟 1 条应取 1 个令牌，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("令牌耗尽后应立即返回 0，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半程不应补满一个令牌，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("满一个周期应补回 1 个令牌，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// Unthrottled channels use a finite cap so one round cannot be held up by unbounded backlog.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("不限流应返回每轮上限 %d，得到 %d", notifyUnlimitedBurstPerTick, got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// Token buckets are independent across channels.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("渠道 1 的桶应仍然为空，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("want=5 时应恰好取 5 个令牌，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// Key assertion: the remaining 95 must still be in the bucket, not drained and discarded.
	// Time is not advanced, so any tokens taken can only come from the existing stock, not from refill.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("剩余令牌应仍可取用（期望 95），得到 %d——桶被整轮抽空了", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("桶已取尽，应返回 0，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	// want<=0 must not deduct any tokens (an empty round is free of charge).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 应返回 0，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 的那次不该消耗令牌，应仍可取满 20，得到 %d", got)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget pins down the two dimensions of digest mode.
//
// Once digest batch size is tied to the per-round request budget, a rate_per_min=20 channel can refill only 1 token
// in each 3-second tick, so every digest message carries just 1 finding — functionally no digest at all, while the
// message header still says 「近 30 分钟新增 1 个漏洞」. This regression raises no error, and the existing  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
// end-to-end tests cannot see it either (they pass a big enough limit to stepDigest manually, bypassing the budget
// calculation in step), so the decision itself is asserted directly here.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// One batch = one message = one request = one token. The token is counted in messages, not findings.
	if tokens != 1 {
		t.Fatalf("汇总一批只发一条消息，应恰好消耗 1 个令牌，得到 %d", tokens)  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("汇总批次大小应为内存上界 db.MaxDigestBatchSize=%d，得到 %d",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			db.MaxDigestBatchSize, claimLimit)
	}
	// Key relation: batch size must be far larger than the per-round request budget. Once they are the same order
	// of magnitude, it means "how many messages to send" and "how many findings fit in one batch" have been conflated into a single number.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("汇总批次大小 %d 不应受每轮请求预算 %d 约束——"+  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			"请求预算是由租约倒推的「发几次请求」，与「一批装几条漏洞」是两个量纲",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
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
		t.Fatalf("单渠道一轮的最坏耗时 %v 不应达到或超过租约 %v"+  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"改这三个常量中的任意一个都要同步检查另外两个",  // allowlist: ordinary Chinese fixture/message text retained; see Task-16 plan
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}

