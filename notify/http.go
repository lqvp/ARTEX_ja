package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether delivery to loopback / link-local addresses is permitted.
//
// Denies by default. These address ranges are not where IM bots or public mail servers appear,
// yet what they can reach is sensitive: another service's admin port on the same machine, and
// the cloud metadata endpoint (169.254.169.254, which can read out instance credentials). The
// delivery address is admin-configured, but a hijacked admin session (via XSS/CSRF) or a
// second person sharing the same JWT can change the config to read response content back —
// doJSON writes the first 200 bytes of a 4xx/5xx response body into last_error, and the delivery
// history API echoes it back, forming a semi-blind read primitive.
//
// But "local SMTP relay" (postfix on 127.0.0.1:25) is a common self-hosted mail setup;
// a blanket block would trap users. So an explicit escape hatch is provided instead of a hardcoded
// allow: setting ARTEX_NOTIFY_ALLOW_LOCAL=1 permits it.
//
// Exported as AllowLocalTargetsEnv so tests can turn it on explicitly — this package and the
// server package use httptest fake receivers on 127.0.0.1 extensively; without it, the guard blocks them all.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an address range that is "not permitted for delivery" by default.
//
// Blocks only loopback, link-local (including cloud metadata 169.254.169.254), unspecified, and multicast.
// Does **not** block RFC1918 private networks: internal self-hosted Mattermost / SMTP relays are common legitimate uses,
// and blocking them would make the feature outright unusable in real environments. This tradeoff is deliberate —
// the defense must block genuinely sensitive targets without taking down normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be converted back to IPv4 before checking, otherwise the check is bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook for the http.Transport dialer, checking the target address
// **at connection establishment**.
//
// Why at the dial stage rather than only at config-save time: this is the final enforcement point.
// It covers two scenarios that bypass config validation — DNS rebinding (resolves to a public IP at
// validation time, resolves to internal at actual connect time) and redirects (though cross-host
// redirects are already rejected, same-host redirects can still point the path elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing delivery to a loopback/link-local address %s (set %s=1 if delivery to a local service is really required)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds only a dial guard on top of the default Transport.
// Uses Clone to preserve all default tuning (connection pool, HTTP/2, timeouts, proxy, etc.),
// avoiding changing other behavior just to add one check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by all channel deliveries.
//
// Deliberately does **not** reuse the project's global egress proxy (GlobalProxy on the server side): that
// proxy is for pentest target traffic and is often an unstable tunnel, while notification availability
// should not be held hostage to target network jitter. IM push can connect directly. Timeout is set to
// 15 seconds — a peer slower than that is effectively already in a failure state.
//
// Rejects cross-host redirects: the delivery addresses in this feature are all "one fixed endpoint" form
// and normally do not redirect to another host; yet the credentials (DingTalk access_token, WeCom key,
// Telegram bot token) are **in the URL**, so following a cross-host redirect hands credentials to the
// redirect target. Same-host redirects (e.g. adding a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps the response body read size. A misbehaving peer can return oversized content,
// and we only need the error code and a short error description for display in delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the response body (already size-limited).
//
// When payload is nil, sends an empty body (for GET or platforms that don't require a body).
// Keys and values in headers are appended as-is, used for custom headers on generic Webhooks.
// Error classification is this function's core responsibility: network-layer failures and 5xx/408/429 are
// classified as "retryable"; other 4xx as "permanent failure" — retrying a 403 just spamms the same error 3 times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (wrong config field type); retrying will not help.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// Invalid URL — most likely the user entered the wrong address; counts as permanent failure.
		// Likewise, err must not be propagated here: url.Parse's error text contains the full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout — mostly transient faults, left to backoff retry.
		//
		// The error text must be redacted before propagating. Reason: http.Client.Do returns a *url.Error
		// whose Error() is `Op "full URL": underlying error`, and the credentials in this feature's
		// platforms are **in the URL** (DingTalk access_token, WeCom key, Feishu hook id, Telegram /bot<token>/).
		// Without redaction, credentials flow down this error stream to four places: notification_deliveries
		// last_error (stored in plaintext), the delivery history API response (**bypassing channel config masking**),
		// server logs, and the 502 text returned to the frontend by the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limit) and 408 (timeout) are worth retrying; other 4xx are config or permission issues where retry is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a single-line short text for error messages. Responses can contain
// newlines and large amounts of whitespace; stuffing them directly into last_error would wreck the delivery
// history page layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses a delivery address into "scheme://host/..." for error messages.
// This package's only address-redaction method, deliberately made **aggressively crude**: everything
// except scheme and host is discarded. The reason is that there is no "universal and safe" way to determine
// which part of a URL is a credential:
//
//	DingTalk   credential in query       /robot/send?access_token=xxx
//	WeCom      credential in query       /cgi-bin/webhook/send?key=xxx
//	Feishu     credential in **path tail** /open-apis/bot/v2/hook/<hook_id>
//	Telegram   credential in **path mid**  /bot<token>/sendMessage
//
// Trying to "keep only the useful parts" means patching per channel, and missing any one is a credential leak.
// Keeping host is enough for troubleshooting (DNS resolution failure, connection refused, certificate mismatch);
// which specific bot is identified via the masked suffix hint in the channel config.
//
// Returns a fixed placeholder on parse failure — never echoes the raw string.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address not parseable)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-layer error, keeping only the underlying cause.
// *url.Error has the structure {Op, URL, Err}, and Error() prints the URL along with it.
// Here we explicitly take the Err field, bypassing its Error() — more reliable than post-hoc string
// replacement, because replacement must correctly handle URL-encoded/escaped variants and is prone to misses.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error errors (e.g. from the redirect policy) can also carry addresses; route through redaction uniformly.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces http(s) addresses appearing in a text with their redacted form.
// Used as a fallback for errors that lack structured fields (redirect policy errors, third-party library custom errors).
// Only recognizes http/https prefixes, splitting on whitespace and quotes — addresses do not contain these characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
