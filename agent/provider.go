// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See the ARTEX architecture design doc
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/lqvp/artex_ja/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType independently controls the thinking "switch" field (thinking.type):
	//   "" = do not send (default, for models lacking this field); "disabled" = explicitly off;
	//   "enabled" = on. Fully decoupled from ReasoningEffort (some endpoints lack the thinking field
	// and can activate thinking via the strength parameter alone, so the two can be set independently).
	ThinkingType string
	// ReasoningEffort independently controls the thinking "strength" field:
	//   "" = do not send (default); "low"/"medium"/"high"/"xhigh"/"max" = corresponding strength.
	//   OpenAI maps to top-level reasoning_effort; Anthropic maps to output_config.effort.
	ReasoningEffort string
	// Stream controls whether this profile uses the streaming (SSE) interface: true (default) = streaming;
	// false = true (non-streaming, send stream:false, full JSON at once via Provider.Complete).
	// Non-streaming can sidestep gateways with poor SSE (empty frames, dropped thinking fields),
	// at the cost of live progress / live token counts. Maps to agentcore.Options.NonStreaming = !Stream.
	Stream bool
	// MaxTokens is the per-reply output cap in tokens. 0 = do not send; server default.
	// Unlike ContextWindowK (total capacity, used locally for compaction thresholds, never sent),
	// this value travels with every request. Maps to agentcore.Options.MaxTokens.
	MaxTokens int
	// MaxTokensField selects the request field name for MaxTokens, effective only for format=openai: "" = max_tokens (default);
	// "max_completion_tokens" = newer field. OpenAI reasoning models (o-series/GPT-5) only recognize the latter;
	// many compatible gateways only the former, so no auto-detection;
	// the user chooses per endpoint.
	MaxTokensField string
	// SessionHeaderKey, when non-empty, makes every LLM request carry a custom HTTP header named by this value,
	// whose value is the current session id (chat=conv-<id>, worker=exp<x>-worker-i<intent>, see WorkerSessionID).
	// Used for gateways keying prompt caches/sticky routing on a session-id header. Empty = do not send.
	// Value attached by transcript.WithSessionID on the request context, read by the RoundTripper,
	// so a shared provider can emit different values per session.
	SessionHeaderKey string
	// Retry holds retry params resolved from this config (profile override -> global policy -> built-in default,
	// resolved server-side). See RetryConfig. Zero value = fall back to built-in defaults entirely.
	Retry RetryConfig
}

// RetryConfig travels with an LLM config. Per-layer "count" semantics: 0 = built-in default count;
// negative = disable that layer; >0 = use that value. Per-layer "interval": 0 = that layer's exponential backoff;
// >0 = fixed interval.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK connect retries (connection reset/timeout/429/5xx before stream starts),
	// mapped to llm.Config.MaxRetries/RetryInterval. Default 3 attempts, 0.5s exponential (cap 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK empty-response retries
	// (completed but no content block, openai format), mapped to llm.Config.EmptyResponseRetries/EmptyResponseInterval.
	// Default 2 attempts, same exponential gradient.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: same-provider safe-window retries added atop the SDK
	// (only replay broken/overloaded/in-stream-429 streams when no output delivered to caller). The SDK never sees this;
	// consumed by server/task_llm.go. Default 2 attempts, 0.5s exponential (cap 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens).
// Below the floor the threshold math (window − summary reserve − buffer) would go non-positive
// and compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction thresholds,
// resolved from user-configured size (ContextWindowK). 0/unset → 200K default;
// otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window in tokens.
// agentcore.NewSession wires the summarizer (same provider)
// when this is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// Default streaming on; ARTEX_LLM_STREAM=false/0/off explicitly disables streaming and routes through non-streaming.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings
// (provider defaults to anthropic; model defaults per provider).
// Inputs are trimmed and the base URL is normalized to the API base the provider expects
// (provider appends endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // default streaming; caller overrides per profile
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off".
// Empty or unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config.
// When a rate is set, the limiter lives on the single provider instance —
// so planner + all workers + main agent (sharing this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// Thinking switch and strength are forwarded independently (empty = that field is not sent).
	// They are decoupled: send only thinking.type, only effort, both, or neither.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// Choice of output-cap field name (empty → use max_tokens). The "value" of the cap does not travel here:
	// it goes per turn via agentcore.Options.MaxTokens; the provider only decides which key to put it in.
	lc.MaxTokensField = c.MaxTokensField
	// Retry params share the SDK semantics (count 0=default/off-negative, interval 0=exponential-backoff/>0=fixed), forwarded verbatim.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance, billing, credit, or quota-exhaustion signals.
// Generic 429/rate-limit text, authentication failures, network errors,
// and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota exceeded",
	// especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary request-rate limiting.
	// Preserve it as an explicit exhaustion signal only when the same error
	// does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context.
	// Empty disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's session id
	// (norma stashes it on the context via transcript.WithSessionID). Stable across a
	// session's turns and distinct across sessions — exactly what a session-keyed
	// prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context
	// so the raw wire bodies can be persisted. This is the only layer that
	// still sees them: norma builds the request body internally and decodes
	// the SSE response before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream
	// that must keep streaming. The 429 branch below reads through this wrapper, so its
	// body lands in the capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // leave blank = direct, no fallback to HTTP_PROXY/HTTPS_PROXY env vars
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a connection test to the server log,
// so "click-test" leaves a diagnosable trail of exactly what the gateway returned —
// 401 bodies, quota text, empty frames —
// not just the collapsed ok/err the UI shows.
// Bodies are clipped to keep a chatty SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — no HTTP request was sent (config parsing or connection setup failed)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — attempt %d/%d HTTP %d\nresponse body: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or the head of an SSE stream
// while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(truncated, %d bytes total)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion
// to verify the provider/model/ endpoint/key actually work.
// Returns the round-trip latency and the model's reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Capture the raw wire transcript: a connection test most needs to see exactly what the gateway returned
	// (status code + response body), which norma decodes into StreamEvent and thus loses.
	// quotaAwareTransport finds this Capture in the context and fills in the status code and body of each HTTP attempt.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// A connection test is a one-shot path, so nobody attaches a session id to the context via agentcore's session loop.
	// For endpoints with a configured SessionHeaderKey (e.g. opencode zen, which requires x-opencode-session
	// or returns 400 MissingSessionID), this creates a gap where "chat works but click-test returns 400".
	// Attach a one-off random session id here so the test goes through the same header-sending logic as a real chat;
	// endpoints without SessionHeaderKey never read it, so there are no side effects.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// Give MaxTokens plenty of budget: reasoning models (e.g. deepseek-v4-pro) emit a large thinking block before the answer
	// (measured: ~2900 tokens even for a "ping"). With only 32, the model stays in the thinking phase and hits the output cap
	// (finish=length), getting truncated; the connection test still counts as success (err=nil) but reads like a messy
	// "interrupted/length/resume" outcome. Give enough budget for OK to come out cleanly (finish=stop).
	// EscalateMaxTokens stays false: do not retry with a raised cap on truncation, avoiding wasted resume loops.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"You are a connection test. Output exactly the two characters OK — do not think, do not explain, do not output anything else."},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // connection test uses the profile's real send/receive mode
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil is not enough: requests that succeed but yield zero visible text do happen in practice (thinking burns the budget,
	// the body gets swallowed by a safety policy, the compatibility layer drops content). Such a config is "does not reply" in a
	// session, yet the test would report success — exactly the gap this check closes. Treat any empty visible reply as failure.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("model returned no reply content (the request succeeded, but no text was returned)")
	}
	return lat, reply, nil
}