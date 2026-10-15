package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform characteristics (which drive the implementation choices here):
//   - Per-bot rate limit of 20 messages/minute; excess is silently dropped (HTTP may still be 200),
//     so rate limiting must be done client-side, see DefaultRatePerMin.
//   - Security settings offer three options: signing / custom keyword / IP allowlist. Signing is the only
//     approach that does not depend on message content, so only signing is supported (a bare webhook with
//     none of the three enabled is also supported).
//   - Both success and failure return HTTP 200, distinguished by the errcode in the body — not checking errcode
//     records delivery failure as success.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's Webhook URL contains an access_token — it is itself a credential, so it is masked entirely.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is DingTalk's Webhook URL itself; changing the address must simultaneously re-affirm the signing secret for the new address.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook address")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

// Send delivers one message. Uses ActionCard (with button) when a back-link is present and it is a single message; otherwise uses markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown body has no explicit byte cap, but a limit is still applied to prevent evidence fields from expanding abnormally.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside a 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is signature verification failure, 310000 is keyword mismatch — both are configuration errors
		// that retry will not heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook per the official signing rules.
//
// Rule: string-to-sign = timestamp + "\n" + secret, HMAC-SHA256's **key is also secret**,
// result is base64-encoded then URL-encoded. timestamp is in milliseconds. When secret is empty,
// returns the hook unchanged, to support bots without signing enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Do not propagate err: url.Parse's error text contains the full address (including access_token).
		return "", fmt.Errorf("failed to parse Webhook address: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that the address is usable, the protocol is supported, and makes an internal-network
// judgment for literal IP targets.
//
// Two points of care:
//  1. **Error messages must be redacted**. url.Parse itself returns a *url.Error whose Error() carries
//     the **full original address**, and the addresses in this feature embed credentials (DingTalk
//     access_token, WeCom key, Telegram bot token, Feishu hook id). This code once did `return err`
//     directly here, so an "invalid address format" error leaked those credentials into the test
//     endpoint's 400 response, the last_error stored on every delivery, server logs, and the delivery history API.
//
//  2. **Literal IPs are judged as internal directly**; domains are left to the dial stage (blockInternalDial
//     is the final enforcement point and also covers DNS rebinding). Doing it here means saving config gets
//     feedback immediately rather than waiting for the first delivery to fail.
//
// Restricting protocols is defensive: file:///gopher:// would cause http.Client to behave unexpectedly (though already blocked by the scheme check, there is no reason to open that surface).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address could not be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing delivery to a loopback/link-local address %s (set %s=1 if delivery to a local service is really required)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
