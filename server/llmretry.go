package server

import (
	"time"

	"github.com/lqvp/artex_ja/agent"
	"github.com/lqvp/artex_ja/db"
)

// Server-side resolution of the retry policy; see docs/llm-retry-design.md. Of the five layers:
//   - connect / empty-response / same-provider safety window follow the endpoint: each LLM profile can override
//     the global default (a blank field on the profile inherits the global value; if that is blank too, the built-in default applies);
//   - circuit breaking / intent replay are process-wide and exist only globally.
//
// Reading the global policy is one settings row in the DB, and every call site sits on a cold path (building a provider, finishing work,
// saving config), so an extra cache layer isn't worth it; the breaker parameters are the exception — they are read on every failure, so
// applyRetryPolicy pushes them into the Registry to hold.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The counts keep their raw "0 = default / negative = off" semantics here: the SDK's MaxRetries /
		// EmptyResponseRetries are shaped exactly the same, so let it parse them itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Circuit-breaker (polling cooldown) defaults, identical to what llmpool builds in — here we only override them when the user actually configured a value.
// Intent-replay defaults live in engine.go: modelErrorRetries / modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob —— the "empty-response
// retry count": the two are the same thing reached by different means. The SDK layer covers "not a single content block" and fixes it by
// re-sending the identical request; this layer covers "only thinking — no body text and no tool call" and fixes it by appending an instruction that lets
// the model continue from the thinking it already produced (a verbatim re-send is meaningless for idling decided by the shape of the context). The emptiness check differs
// because the SDK counts "did we ever yield an event", while thinking deltas are events themselves — but when a user sets
// "empty-response retries" they mean "run it once more when the model produced no substantive content", so sharing one count
// between the two layers is what matches that mental model.
//
// Reads the global policy rather than some profile's override: a run may switch profiles mid-flight on failover, and this is
// the total gate for the whole intent, so it must not change with the endpoint. Semantics match the SDK's emptyRetries():
// 0 = default defaultEmptyTurnNudges; -1 (negative) = turn empty-turn continuation off; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
