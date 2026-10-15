package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard limit for WeCom group bot markdown content (bytes, not chars).
// This is the tightest limit among all six channels and the main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group bot. Platform characteristics: - The only channel that authenticates via key in the URL, no signing — so the
//     webhook address itself is the entire credential.
//   - markdown content limit is 4096 **bytes**; over-length is rejected entirely (not truncated).
//     Chinese is 3 bytes/char, meaning only ~1300 chars are available for the body — client-side truncation is mandatory.
//   - Rate limit 20 msgs/min, also enforced by client-side rate limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential (the key in the URL), and it doesn't support
// signing — the entire address is the full credential, no other fields need masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only one field (webhook), which is both destination and credential,
// so there's no "residual credential after address change" to speak of.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook address")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook address: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// Batch messages can be very long (50 items × one line each + prefix), easily exceeding 4096 bytes.
	// Truncation is done here rather than relying on platform errors: rejection means the entire batch is lost,
	// while truncation at least delivers the first several items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 is API call rate limit — the platform's rate limit window rolls, so backoff+retry is effective,
		// hence explicitly classified as retryable. Reaching here means the client rate_per_min is configured too
		// aggressively; retry is just a backstop, the real fix is lowering that channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is invalid webhook key — permanent failure, retry won't self-heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
