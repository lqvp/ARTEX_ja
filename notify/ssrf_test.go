package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardening measures: ① Delivery addresses must not use the server as a pivot to reach
// internal networks / cloud metadata (SSRF) ② Address validation error messages must not leak credentials from the address
//
// Test environment note: many tests in this package use httptest fake receivers on 127.0.0.1, which the guard blocks by
// default. So TestMain globally enables AllowLocalTargetsEnv, and each SSRF test case explicitly clears it to assert the
// **default-deny** behavior.

func TestMain(m *testing.M) {
	// Let regular tests connect to local fake receivers; SSRF tests clear it themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion: by default, delivery to loopback addresses must be
// rejected at the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("delivery to loopback addresses must not be allowed by default")
	}
	if hit {
		t.Fatal("the request reached the local service — the guard did not take effect")
	}
	// Error message should guide users on how to opt in (local SMTP relay is a valid config).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to opt in explicitly: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the reverse case: once explicitly enabled, it must work — otherwise legitimate
// deployments like local postfix / internal relay would be broken.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work after explicitly opting in: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint — the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be normalized before checking, otherwise it's a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private networks are **deliberately allowed**: internal Mattermost / SMTP relays are common legitimate uses.
	// This assertion pins that tradeoff — if someone later adds private-network blocking, this test fails, forcing a conscious
	// decision rather than silently breaking deployments.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private networks are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the pre-delivery hint: literal IPs should be rejected at save
// time, not at first delivery failure.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at config time", raw)
		}
	}
	// Public and private addresses pass as usual (private is left to the dial stage, which doesn't block).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch I missed in the previous round, as flagged in audit.
//
// When url.Parse **fails** it returns *url.Error whose Error() contains the full original address. In the previous round I
// only sanitized the error from http.Client.Do and missed this; the "permanent failure path" cases I added then (file://,
// gopher://, ftp://) can actually be parsed successfully by url.Parse and take the scheme branch, so all-green proved
// nothing about this path — it was a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // invalid percent-encoding
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unbalanced brackets
	}
	for _, raw := range cases {
		// First confirm this input **does** make url.Parse fail. Without this step, the test might unknowingly take a different
		// branch (that's how the previous round's false guarantee happened).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should have failed to parse; otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel-level wrapper also doesn't leak the address.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP channel's dial guard.
//
// The email channel used to use a bare net.Dialer — the only gap in the entire SSRF defense: setting host to
// 169.254.169.254 or 127.0.0.1 could connect directly, and when smtp.NewClient's handshake fails it wraps the peer's
// response line into the error, which gets echoed via last_error through the delivery history API — exactly the half-blind
// read primitive that other channels have already closed; the timing difference between "connection refused" vs "timeout"
// can even be used to probe ports.
//
// This package's TestMain globally enables AllowLocalTargetsEnv (many tests use fake receivers on 127.0.0.1), so this test
// must clear it itself — otherwise it would pass regardless of whether the guard exists, which is exactly why the gap was
// never caught by any test.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("mail delivery to loopback addresses must not be allowed by default")
	}
	// The connection must never be established: the guard blocks in the Control hook, so EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP session was established — the guard did not take effect")
	}
	// Error message should guide users on how to opt in (local postfix relay is valid).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the rejection message should explain how to opt in explicitly: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired reverse case: once explicitly enabled, delivery must work.
// Internal SMTP / local relay is a very common deployment; the guard must not be a blanket block.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP delivery should work after explicitly opting in: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO not seen — the session was never really established")
	}
}
