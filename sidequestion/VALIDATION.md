# `/btw` Validation Record

Date: 2026-09-10. Branch: `codex/btw-side-question`. Baseline: `8dae851b9b622f2ff2631f332fde9719d0b16fba`.

Uses an independent PostgreSQL test database and data directory; real model credentials were injected only into the independent test environment and were not written into code or this record, and the product default model was not modified. Go 1.26.3, norma v0.3.6, Next.js 16.2.9.

Actual model conversations, returned objects, engineering assertions, and the raw Qwen review text are stored in [validation-2026-09-10.json](validation-2026-09-10.json), which contains no API credentials.

## Engineering Checks

| Scope | Result | Evidence |
| --- | --- | --- |
| Structured messages, tool parameter deep copy | Pass | `TestCheckpointDeepCopyAndBoundaries` |
| Summary/compaction requests do not overwrite; complete replies and terminal state published; partial replies excluded | Pass | `TestCheckpointDeepCopyAndBoundaries`, `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Actual model pool member identity | Pass | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| Tool pairing, 20-pair replay, budget trimming and over-limit errors | Pass | `TestBuildRequestCompactionToolPairingAndBudget` |
| Main/side parallelism, bidirectional cancellation isolation | Pass | Blocking Provider, `TestMainSideConcurrencyAndIndependentCancellation` |
| No tool execution, streaming/non-streaming, existing usage on failure | Pass | `TestServiceNoToolsAndUsageOnFailure` |
| Real norma ChatAgent + local Read tool, main transcript/activity isolation | Pass | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`, streaming and non-streaming subtests |
| Persistence, pagination, idempotency, partial-answer retention across restart | Pass | `TestSideHistoryIdempotencyPagingAndRecovery` |
| Clear and late-write races, parent resource deletion, version comparison | Pass | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent/Worker archiving and recovery, v1/v2/v3 | Pass | `TestSideTaskArchiveVersions` |
| Three parent interfaces, authentication, resource ownership, Worker logical deletion | Pass | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`, `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Busy main session can be bypassed, independent SSE reconnect/disconnect, cancel, clear | Pass | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 1 per parent session / 4 global concurrency | Pass | Two `TestSideHTTP…` cases |
| Snapshot persisted before admission, follow-up question across restart, old sessions cannot forge snapshots | Pass | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| Cached configuration deleted or model changed rejects continuation | Pass | `TestSideRejectsDeletedOrChangedCachedProfile` |
| Cancel before archive and wait for final answer and usage persistence | Pass | `TestSideTaskDrainPersistsBeforeArchive` |
| Streaming consumer cancels early recording usage only once with side attribution | Pass | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| Auto-recovered Worker/deadline runtime contexts continue publishing new snapshots | Pass | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| Race checks on related packages | Pass | Commands below |
| TypeScript and production build | Pass | `npx tsc --noEmit`, `npm run build` |
| Biome on new frontend modules | Pass | `biome check`, 3 new modules |

After configuring `ARTEX_PG_DSN` in a separate disposable database, the automated checks can be reproduced (do not point at a production database):

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

The full Go regression is not all green: two pre-existing tests in the `server` package fail during the temporary-directory cleanup phase, both reporting `TempDir RemoveAll … directory not empty`:

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

After exporting source from the above unmodified baseline, rerunning the `server` package in the same isolated environment also reproduces these two cleanup failures. The baseline run additionally showed a `TestCoreTaskLifecyclePG` target node count assertion failure; the final modified `server` regression does not show that assertion failure. Other packages pass, and the side-question-related cases and race checks pass. The baseline issues were not marked as passed for this acceptance, and existing assertions were not modified to hide problems.

The Next.js build output has pre-existing multi-lockfile / workspace-root inference warnings; the build completes and all pages generate successfully.

## Browser Checks

Using the Codex In-app Browser, connected to an independent local Go service and Next.js dev server. On desktop and a 390 × 844 narrow screen, completed the following manual-automation operations, checking screenshots and browser logs:

- Typed `/btw` during a normal chat run; main content and side question displayed simultaneously; desktop sidebar normal.
- Consecutive follow-up questions; after stopping the side question, the generated part is retained; the main flow continues.
- Request continues when the panel is closed, and the completed answer is restored on reopening; after a page refresh, an empty `/btw` restores history.
- The narrow-screen Drawer's input, buttons, history, and close operations are normal, with no horizontal overflow.
- Clearing uses a confirmation dialog; after clearing, history disappears, and the main transcript and snapshot are retained.
- Asked questions separately from the task MainAgent and two Workers and switched between them; Agent labels and history did not cross over.
- A blocking local model fixture keeps the Worker running; submitting `/btw` from the Worker's main input, after stopping the side question the Worker still shows live running and its own pause button, and the side question saves a partial answer.
- Browser error/warning logs are empty.

Controllable fixtures were used to precisely verify concurrency timing without depending on real model output speed. During debugging, two Worker runtime checks did not form a valid concurrent window (the task had already ended / the answer ended early); after fixing the fixtures, they were redone and passed; these initial operations are not counted as valid passes.

## Real Model Conversations

Probed `grok-4.6` first, OpenAI-compatible endpoint `http://127.0.0.1:12580/tingly/openai`. The probe returned HTTP 200, model name `grok-4.6` and `READY`, in 2.82 seconds. The first choice was available, so the Tingly `glm` and Zhipu `glm-5.3` fallback chains were not enabled; these two fallback services were not validated this time.

| Scenario | Actual result |
| --- | --- |
| Asked about assets, goal, and flags while the main session was running | Returned `redhaze.top`, homepage read and goal summary, `BTW-REAL-0910`; side question completed in 16.97 seconds |
| Asked about tool basis after the main session completed the homepage read | Correctly cited WebFetch 200, curl redirect 301 → 302 → 200, and the page title; 7.24 seconds |
| Side question asked Bash to create a test file | Refused to execute; the target file was not created; 7.74 seconds |
| A completed side question does not change the main context | Main transcript SHA-256 matches the main activity record; side-question tool execution count is 0 |
| Follow-up question after actually stopping/restarting the Go service | Retained the previous 3 side-question history entries and answered assets, flags, and title directly from the persisted snapshot, without re-running the main Agent |
| New session using Grok non-streaming configuration | Correctly answered assets and `ATOMIC-0910`; returned and saved usage: input 11734, output 138, cache_read 11520 |

For the asset case, the main session used WebFetch and Bash/curl to read the public homepage; the landing page is `https://id.redhaze.top/home` with the title "红幕科技 RedHaze Group · 全球综合集团门户". Bash staged the response in a local test file; no writes were executed to the remote. This fact is verified separately from "the side question did not execute tools".

Main transcript checksum: `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00`.

**Usage limitation:** Tingly's Grok streaming responses did not return usage. A separate direct request with `stream_options.include_usage=true` was sent to verify: HTTP 200, 12 data frames, 0 usage frames. Therefore the 0 in streaming tests means the endpoint did not provide usage and cannot be interpreted as no billing. Non-streaming usage and the fixture's failure/cancel usage were all saved correctly.

## Qwen Review

Review model `qwen-flash`, OpenAI-compatible endpoint `https://dashscope.aliyuncs.com/compatible-mode/v1`, HTTP 200. Provided the first three real side-question conversations, the main session's tool basis, and engineering assertions; returned `verdict: accept`, `concerns: []`, considering the answers consistent with the assets, flags, and page-read evidence, and the side-question tool refusal compliant with constraints. Review usage: prompt 6625, completion 312, total 6937.

This Qwen review scope does not include the later appended service restart and non-streaming tests. Qwen's generalization of "no writes" was too broad: the main session's curl did create a local response temporary file, as explicitly recorded above. Concurrency, zero tool execution, and transcript isolation are judged by engineering assertions; the model review only assists in evaluating answer quality.
