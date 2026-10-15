# Side-Question Long Conversations and Context Budget

Fixed 2026-09-11. The original implementation treated the request JSON character count as tokens and inherited the main task's 32K output reservation, so normal side questions were rejected early when there was a lot of HTML, JS, and tool results.

## Open-Source Implementation Review

- [Grok CLI side-question context](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/agent.ts#L739): extracts snippets from recent user/assistant text, character budget about 2000, at most 400 characters per entry. Does not maintain a continuous side-question history on this path.
- [Grok CLI standalone request](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/utils/side-question.ts): independent cancel signal; output cap 2048 tokens when the model supports it; no tools.
- [Grok CLI main session compaction](https://github.com/superagent-ai/grok-cli/blob/fb97af83f06dca873281d60168430f06c8de6324/src/agent/compaction.ts): estimates tokens, retains recent content, updates new content into the old summary, and handles cross-turn truncation.
- [OpenCode session compaction](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/compaction.ts): full request estimation, output/buffer reservation, recent content plus rolling summary, tool-free summary request; the implementation defaults to a recent budget of 8000 and a summary output cap of 4096 tokens.
- [OpenCode overflow recovery](https://github.com/anomalyco/opencode/blob/b3f1a96c6dd7adeb28b36dd11add1998fc84d67b/packages/core/src/session/runner/llm.ts): overflow recovery is attempted only before assistant output has begun; the recovered call does not re-enter the same overflow recovery path.

ARTEX borrows the independent output budget, recent content with rolling summary, and limited recovery approaches. It keeps norma v0.3.6's structured messages and tool pairing, does not copy Grok's text extraction, and does not write OpenCode's main-session compaction events into the ARTEX main transcript.

## Request Budget and Execution

- Messages use norma's UTF-8 byte estimation per content block with a 4/3 margin; system prompts, tool schemas, and message envelope overhead are additionally counted. The estimate is not the model's exact token count.
- Side-question output defaults to at most 8192 tokens and never exceeds the output cap already set by the main configuration. The service environment variable `ARTEX_BTW_MAX_OUTPUT_TOKENS` sets a 256–32768 cap; it does not modify the product default model or main-task parameters.
- The input budget is the context window minus the output cap and safety margin; unknown windows use the platform default 200K. The safety margin is 5% of the window, minimum 128, maximum 8192 tokens.
- Successful Q&A is loaded in batches of at most 20 pairs with increasing ordinals. At most 20 original pairs are kept, with a token budget of at most 1/4 of the input budget and no more than 16K.
- Over-budget Q&A is updated into a rolling summary. The summary carries historical source and context time; historical assistant answers are not equivalent to new tool evidence; on conflict the latest main snapshot takes precedence.
- When the main context is still too long, only old messages in the summary copy are summarized, retaining at most 8K tokens of recent content; truncation points never split tool calls from results. An oversized single pair enters the summary whole.
- Summary input is UTF-8-safe chunked by the actual remaining window, with an output cap of 2048 tokens; empty summaries, truncation, tool calls, or exceeding the summary budget do not write a cache. A single side question allows at most 12 summary calls, subject to the same 120-second timeout; reaching the cap fails explicitly and never loops forever.
- If the model first returns a context-overflow error and no text or tool call has been output yet, it retries at most once after further reduction; if the estimated size does not decrease, recovery stops immediately. Other model errors and partial streaming output do not trigger this recovery.
- All usage obtained, including summaries, failed attempts, and usage at cancellation, accumulates into the same side-question request. When the provider does not return usage, only zero can be recorded; estimates must not be disguised as actual usage.

## Persistence and UI

`side_question_sessions.memory` stores old Q&A summaries, covered ordinals, and cached main-context summaries keyed by snapshot identity. `side_question_requests.context_info` stores the preparation phase, actual replayed pair count, summary usage, and budget estimates.

Summaries are saved only while the original request is still running and the cleanup version matches; clearing also removes the cache, and late writes do not restore already-cleaned data. New snapshots do not reuse old snapshot summaries. Summary fields are saved with v3 task archives; when restoring old v3 records missing new fields, empty objects are filled in, and v1/v2 remain compatible.

POST first admits and returns the request; preparation and compaction run in the background without holding the admission lock or a database transaction. SSE/history display the preparation, organizing Q&A, compaction copy, and answer phases; compaction failure is saved as the failed terminal state of that side-question request. The frontend keeps the error and restores the draft of the failed question, without a floating notification covering the input box; history polling no longer clears submission errors.

## Validation Record

- 19/20/21/50-pair replay, retention of old conclusions across 20 pairs, summary cache reuse across restarts: automated pass.
- Oversized Chinese answers and code context, chunked request budgets, tool pairing, snapshot immutability, new-snapshot cache invalidation: automated pass.
- Summary failure/cancellation/truncation/oversized/tool returns, clear races, call caps, single-only overflow recovery, and no retry on partial stream: automated pass.
- Independent PostgreSQL pagination, restarts, v1/v2/v3 archives, summary cache and budget metadata archive recovery, old v3 missing new fields, 20 parent sessions sharing four concurrent slots: pass.
- Go candidate service build, frontend TypeScript check, Biome check on modified components, and a Next.js production build in an independent directory: pass.
- Built-in browser used independent UI fixtures at 1280×720 and 390×844 to verify the organizing phase, summary scope hints, draft recovery after failure, no floating error notification, no horizontal overflow, and no console errors. Temporary fixtures have been removed.
- Read-only replay of local existing Worker snapshots passed the new budget checks; for example, Worker #3's 293085-character snapshot is no longer misjudged and rejected by local character counting. This item made no external model calls.
- The real-conversation test sending private Worker snapshots to Grok was rejected by automated approval review, was not executed, and is not counted as a pass.
- On 2026-09-11 00:37, restarted the local backend at the user's request, reusing the original database, data directory, and login configuration. The running file and candidate binary SHA-256 match; both the backend and the frontend proxy `/api/health` returned normal.

Validation commands (using only independent test databases):

```sh
go test -race ./sidequestion ./db ./server -run 'TestSide|TestCheckpoint|TestSnapshot|TestBuildRequest|TestService|TestMainSide|TestTaskArchive' -count=1
go build ./cmd/artex
npx tsc --noEmit
npm run build -- --webpack
```

The frontend production build uses an independent copy to avoid overwriting the current preview `.next`. The candidate service is at `/private/tmp/artex-btw-budget-candidate`, copied to `/private/tmp/artex-btw-preview/artex` and started; the original binary is backed up as `artex.before-context-budget` in the same directory.
