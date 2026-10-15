package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be **stateless**:
// one instance is shared concurrently across multiple channel configs; credentials arrive via cfg.
type Channel interface {
	// Kind returns the channel type identifier; it must match the registry key.
	Kind() string
	// Validate is called when saving a config; it checks required fields and formats.
	// Returned errors are shown directly to the configurator, so messages must name the missing field, not a vague "invalid config".
	Validate(cfg map[string]any) error
	// Send delivers one message and returns the **number of items actually delivered** plus error.
	//
	// Why return a count: every platform has a message length cap; batched messages may be truncated when they exceed it.
	// If the caller unconditionally marks the whole batch delivered, truncated items vanish — unseen in messages,
	// shown as successful in delivery history, with no way to discover they were never sent. Returning kept,
	// the caller marks only the first kept items delivered and leaves the rest for the next batch.
	//
	// An error means delivery failed; *PermanentError means it should not be retried.
	// On failure, kept is meaningless and the caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the channel's official suggested per-minute delivery cap, used as the
	// default rate limit for new channel instances. 0 means no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the config keys that hold credentials. Their values are masked in API responses;
	// on update, a masked value keeps the stored original. Only the implementation knows which fields are credentials
	// (WeCom's entire webhook URL is a credential, while DingTalk's is only the secret within it);
	// so this knowledge must be provided by the channel, not guessed by upper layers.
	SecretKeys() []string
	// DestinationKeys returns config keys that decide "where the message goes".
	//
	// Like SecretKeys, this is security-related: destination and credentials are independent fields,
	// Allowing "change address only, keep credentials" lets anyone who can edit channel config send the stored
	// real credentials to a server they control, making config masking meaningless.
	// See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. It deliberately uses an explicit literal instead of init() self-registration:
// all channels are visible in one place, and a new channel is caught at compile time rather than by runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for kind.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (stable for UI dropdowns).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: credential errors, target rejection, invalid body, etc.
// Retries only make sense for transient faults (network jitter, rate limiting, peer 5xx); retrying permanent failures
// never succeeds and buries the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// convenient for writing `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err (or its chain) carries a permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config read helpers ----
//
// Channel config comes from a JSONB column in the DB, deserialized by encoding/json into map[string]any;
// numbers are all float64 and arrays are []any. These helpers normalize that conversion and tolerate the type
// deviations users cause by leaving fields blank in the UI (e.g. entering a port as a string).

// cfgString reads a string config item, trimming surrounding whitespace — easily picked up from copy-pasting web form values.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an int config item, accepting both float64 (JSON default) and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a bool config item, accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config item, trimming entries and dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accepts a single string, convenient for form submission of one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config item (e.g. custom HTTP headers), trimming keys/values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
