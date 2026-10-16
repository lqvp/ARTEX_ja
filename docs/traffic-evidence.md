# Vulnerability Multi-Traffic Evidence

The vulnerability detail page's "Associated Traffic" supports cross-page multi-select, roles and notes, sorting, and unbinding. The traffic page can also multi-select records and associate them with an existing vulnerability in one action. Task-inherited vulnerability evidence is read-only; modifications require entering the source task.

The "Agent Auto-Bind Traffic" setting in System Settings is off by default. The read/modify endpoint is `agent_traffic_binding` under `/api/settings`. Enabling it increases Token consumption from reading requests/responses, tool calls, and prompts; the next Agent round uses the new setting. When off, auto-bind parameters and the rebind tool are hidden, auto-bind guidance is not injected, and auto-binds newly submitted to an already-running session are rejected. Manual binding, traffic capture, saved-evidence reads, and exports are unaffected.

When enabled, the default flow is "finding stored → auto-trigger report Agent → verify and bind traffic → write report at the latest evidence version". The reporter keeps verification commands, key output, existing real traffic IDs, and roles in `evidence`; the report Agent combines the vulnerability detail and execution records, verifies with `traffic_search` / `traffic_get`, calls `bind_finding_traffic`, then reads the latest `version` and saves the report. When the switch is off, the report Agent is not given raw traffic search/read tools and does not auto-bind; it can still read manually-bound snapshots and generate reports.

Compatible with existing callers: `report_finding`'s `traffic_refs` / `evidence_hint_id` still support explicit immediate binding. Binding is optional: for non-HTTP vulnerabilities such as TCP, or when no traffic was collected or no exact record is found, omitting references still allows normal reporting and writing; keep other verifiable evidence such as command output and logs, and it is recommended to explain why no binding was made — no new required fields are added. All submitted IDs must be valid with complete bodies. Any single failure rolls back the entire binding operation; an explicit bind submitted with a report fails, rolling back the whole report. Re-appending the same snapshot does not add a binding and does not overwrite the note.

## Agent Numbering and Report Version

`report_finding` gains optional parameters; array order is the initial evidence order:

```json
{
  "traffic_refs": [
    {"traffic_id": "real traffic ID", "role": "baseline", "note": "normal account request"},
    {"traffic_id": "another real traffic ID", "role": "proof", "note": "reproduction request"}
  ]
}
```

Roles are `baseline` (normal control), `proof` (vulnerability proof), `verification` (supplementary verification), and `supporting` (supporting evidence, default). First verify real records with `traffic_search` / `traffic_get`; domains and times are only for candidate filtering and do not presume task ownership.

The prompt references [CyberStrikeAI's vulnerability reporting tool guidance](https://github.com/RuoJi6/CyberStrikeAI/blob/54d56774b8bd285817d16d48b70a4a5e6e0963f7/internal/app/vulnerability_tools.go), combined with this project's optional binding conventions; no mandatory validation is introduced for missing-package reasons. Do not guess IDs, and do not re-probe merely to fill packages.

The first return line remains `finding recorded: <exploration node ID>`; the following JSON provides the standalone finding record `finding_id`, the exploration node `finding_node_id`, and a binding summary.

- `get_finding_traffic(finding_id)` uses the **standalone finding record ID** and returns an ordered list with `version`; passing `binding_id`, `side=request|response`, `offset`, `length` reads in segments of at most 8192 bytes each.
- `update_finding_report`'s `finding_id` **continues to use the exploration node ID**. The new `evidence_version` is filled with the actually-read version; if evidence changes during generation, stale-version writes are rejected and must be re-read and regenerated.
- When an old report call does not pass a version, it does not claim to overwrite existing traffic evidence. After binding, notes, roles, or ordering change, the existing report is prompted as pending update.

With auto-bind enabled, `add_hint` / `add_task_hint` support saving `traffic_refs` in a single hint or in each element of a batch `hints`. The planner reporting on behalf can pass `evidence_hint_id`, explicitly selecting references from this task's corresponding hint; inherited hints cannot be referenced. The system does not guess bindings by domain, time, or browsing history. A failed submission does not produce partial findings or prematurely trigger a report.

Use `bind_finding_traffic(finding_id, traffic_refs)` to backfill bindings for existing vulnerabilities without re-registering. `list_findings` / `list_task_findings` / `node_detail` / `get_task_node_detail` return explicit `finding_id` and `finding_node_id`; the legacy `id` retains exploration-node semantics.

At startup, only optional properties are added to old tool schemas; the original default traffic tool binding is extended to the report Agent, and the rebind tool is given to the report Agent by default. Custom binding lists, prompts, descriptions, and enabled states are preserved. Guidance is added uniformly after final tool assembly: the reporting role hands off existing evidence, and the report Agent is responsible for verification, binding, and writing the report. When a platform conversation lacks task context, it should be handed off to the task Agent via a structured prompt, not reported directly. Before judging a task complete, existing evidence should be handed off first; missing packages do not force waiting. A failed `report_finding` does not trigger the report Agent.

## API

Base path: `/api/exploration/findings/{finding_id}/traffic`, using the standalone finding ID. Authentication is reused; `context_task` validates task visibility and inheritance read-only.

| Method / relative path | Request / response |
| --- | --- |
| `GET` | Ordered summary, evidence version, report-adopted version |
| `POST` | `{"traffic_refs":[...]}` appends a whole batch |
| `PATCH /{binding_id}` | `{"version":1,"role":"proof","note":"note"}` |
| `DELETE /{binding_id}` | `{"version":1}` |
| `PUT /order` | `{"version":1,"binding_ids":["2","1"]}`, must be a complete list |
| `GET /{binding_id}` | Snapshot metadata and bounded body preview |
| `GET /{binding_id}/body` | `side`, `offset`, `length`; `download=1` downloads the complete original bytes |

Version/sorting set conflicts and writes during archive return `409`; inherited writes return `403`; bindings that do not exist or do not belong to the vulnerability return `404`; traffic/attachment reads and validation failures return explicit errors.

## Storage and Migration

Startup performs idempotent PostgreSQL migrations: new `traffic_evidence_snapshots`, `finding_traffic_bindings`, and `findings.evidence_version` / `report_evidence_version` (default 0). Historical text is not guessed for backfill bindings.

Snapshots store the original traffic ID, capture time, URL, method, status, request/response headers, body length, and SHA-256. Bodies are stored by hash under `<data>/evidence/blobs/<first-two-chars>/<hash>.bin`, independent of the cleanable `data/traffic`; multiple vulnerabilities can share snapshots/bodies. Snapshots provide no content-update interface; read and export fail on verification mismatch.

The complete body is read under the original traffic write lock, including large-body blobs and legacy directory records. First persist and verify the file, then write the exploration node, intent relations, vulnerability, snapshots, and bindings in a single PostgreSQL transaction; only after commit is the planner notified. Failure may leave unreferenced files but never partial business records.

PostgreSQL advisory lock `7337741004` coordinates evidence files and SQL references; a task row lock forbids evidence modification after archive queuing. Recovery holds the evidence lock from body installation through metadata commit. Deleting a vulnerability cascades to remove bindings.

The cleaner runs hourly, reclaiming only unreferenced content not in an in-progress operation, with at least a 24-hour delay. Normal traffic cleaning does not touch the evidence directory. When backing up hot data, back up both PostgreSQL and `data/evidence`.

## Export and Archive

Markdown includes an ordered evidence list with versions; JSON includes metadata; CSV adds counts and binding IDs. `md-zip` retains the vulnerability Markdown and provides:

```text
evidence/<finding_id>/<binding_id>/
  manifest.json
  request.http
  response.http
  request.bin
  response.bin
```

Markdown references messages via relative links. Before sending a download, attachment copying, hashing, compression, disk sync, and CRC read-back verification of all ZIP entries are completed; missing or corrupted entries fail the entire download. Complete attachments retain binary original bytes.

Archive v3 collects snapshots and bodies by vulnerability binding relationship, independent of original traffic or domains. Hot data is cleaned only after package verification completes; shared evidence remains. Recovery first verifies and installs bodies, then transactionally restores metadata and bindings, supporting failure retries. v1/v2 remain restorable, with missing new fields explicitly defaulted to 0.

## Validation and Boundaries

For each test package, set up an independent, newly created PostgreSQL test database, specified via `ARTEX_PG_DSN`, to avoid leftover task/model fixtures triggering background runs. Run the full tests for the relevant packages and confirm there are no skips due to missing configuration:

```sh
# Before each package run, set ARTEX_PG_DSN to the corresponding independent test database;
# explicit configuration failure must error.
go test ./<package> -count=1
go test -race -p 1 ./evidence ./db ./agent ./server -run 'TestEvidence|TestFindingTraffic|TestFindingEvidence|TestReportFindingAtomicContract|TestTaskArchive'
```

Frontend validation includes `npx tsc --noEmit`, Biome checks on affected files, Webpack builds, and `NEXT_EXPORT=1` static export. Use an independent cache directory to avoid overwriting a running dev service.

Local end-to-end acceptance uses independent ports, controlled HTTP / domain HTTPS targets, and a temporary data directory, covering both binding entries, cross-page selection, sorting/notes, error messages, inheritance read-only, downloads, and after deleting original traffic: export, archive, hot-body reclamation, recovery, and hash verification.

The first version uses a global evidence coordination lock; during large-batch binding/export or slow attachment downloads, other evidence operations may wait. Evidence must not be fabricated when capture records or complete bodies are missing. This feature does not change the capture switch and does not handle certificate issues for direct HTTPS access to IPs.
