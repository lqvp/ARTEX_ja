---
name: api-recon
description: Call this skill when collecting a website's API endpoints.
---

# API Recon (frontend endpoint reconnaissance)

Under an **authorized** premise, discover as completely as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature trigger points** (tabs, dialogs, table operations, etc.).

---

## Boundaries and Prohibitions (Agent must read · violations are out of scope)

This skill **only does API / parameter-surface reconnaissance**; it is not the vulnerability discovery or penetration-exploitation phase.

### Task Boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate path, method, parameters, routes, UI trigger points | SQLi/XSS/privilege-escalation/brute-force/fuzz vulnerabilities, packet-modification attacks, destructive operations |
| **Auth** | Hook + stub/mock to bypass the **client-side** login gate | Asking the user for or guessing account credentials; attempting real login form submissions |
| **Runtime** | Hook interfaces without credentials, use mock responses to let the SPA enter the post-login shell | Flows that depend on a real backend session to proceed |

### Credential-less Dynamic Analysis (default for Phase 3)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap interfaces such as login, permissions, and menus;
2. Return mock bodies for business query interfaces with **correct structure, successful business codes, and possibly empty data**;
3. Make the frontend still render post-login pages without a backend or under 401, thereby triggering more XHR/fetch/WebSocket;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to real login or vulnerability testing for these.

**In one sentence**: use mocks to prop open frontend routes and component mounting, **only record outbound requests**; what the backend returns doesn't matter — what matters is which interfaces the frontend **still sends**.

### Hard Process Prohibitions

| Prohibited | Alternative |
|---|---|
| grep/curl/Read the main entry `index-*.js` to extract API paths before Phase 1 completes | Run `OUTDIR/harvest_static.py` |
| Hand-writing replacement scripts such as `extract_apis.py` for harvest | Modify `OUTDIR/harvest_static.py` and rerun |
| Repeating the same failing grep/command ≥2 times | Switch strategy: read tool_logs, modify harvest, check reference |
| Skipping gates A/B and running the originals in `scripts/` | Copy to OUTDIR and adapt to the target |
| Real usernames/passwords, OTP, OAuth, etc. | stub/mock (see above) |
| Skipping stubs on the pretext of "getting real data" to do privilege-escalation/injection tests | Only record outbound; that's the recon boundary |
| Deleting, exporting sensitive data, batch writes, or other irreversible operations | Same for coverage clicks |
| Claiming all pages and interfaces obtained without runtime + dynamic enumeration | See "Definition of Done" or mark limitations |
| Claiming mastery of all parameters without the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff or validation rules/error back-inference |

---

## Two-Layer Model + Run Modes

| Layer | Output | Limits |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, route drafts, bundle-point field candidates | No HTTP methods; parameters need Phase 1b; misses runtime-concatenated URLs |
| **Runtime** (live session) | Methods + body + responses + dynamic URLs + WS/SSE; multi-sample diff fills in parameters | Pages must actually render to send requests; a single sample cannot determine required/optional |

| Run mode | Engine | Applicable |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/dialogs/tables for deeper feature-point coverage |
| **both** | depth first, then coverage | Most complete, longest runtime |

**Parameter methodology** (no generic script): path uses harvest/regex; parameters use **anchor windowing + UI binding chain + multi-sample diff + error back-inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of Done

All must be satisfied before claiming recon is complete:

- [ ] **Static**: Phase 1 harvest produces `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **Hook in effect + dynamic enumeration loop**
- [ ] **Entered shell**: non-`/login` when visiting business paths (mind hash routing)
- [ ] **Parameters**: coverage/both complete the parameter trigger matrix + `param_samples.json`; Phase 5 merges `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission-tree restoration and rerun until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Deliverables**: Phase 5 outputs complete (see Phase 5 output table); `insert_assets` writes service and endpoint assets

---

## Scripts and Gates

`scripts/` are reference templates only; **it is prohibited** to run the originals directly and treat them as final results.

**Rules**: read first → adapt to the target → write to `OUTDIR` (e.g. `recon/`) → record in `CHANGES.md`; if nothing matches, rewrite using the methodology and borrow only the structure.

| Gate | When | Reference script → OUTDIR copy | Common must-change items |
|---|---|---|---|
| **A (static)** | After Phase 0, **before the first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Most sites' default regex runs directly**; only adapt the endpoint regex, webpack/Vite `publicPath`, or MPA exclude/cookie when manifest/dialect doesn't match |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**SPA mandatory order** (non-interchangeable; Phase numbering takes priority over "explore first, then script"):

| Step | Must | Prohibited |
|---|---|---|
| After Phase 0 | Next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → small adaptations as needed → **run immediately** | Manually extracting APIs first and then deciding whether to harvest |
| Before Phase 1 completes | `wc -l` to validate outputs; 404 → modify harvest and retry | Hand-written extract scripts; repeated grep of undeclared URLs |
| From Phase 1b | grep only `OUTDIR/js/*.js` | Using the main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → (optional) modify regex → **run immediately**
- ❌ curl main bundle → grep repeatedly → write a temporary extract → only then harvest
- **MPA**: after Phase 0, the next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` >100KB **must not** be Read/grepped into context; use OUTDIR scripts for batch processing |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in the conversation, don't paste bundle snippets |
| Validation | Use `wc -l`, `ls \| wc -l`; don't Read entire directories |
| regex first probe | Optional, ≤1 time, only ≤50KB small chunks or HTML; official static results come from harvest |
| reference | Recipes/templates/troubleshooting in [reference.md](reference.md); don't repeat the full text inline |

---

## Execution Roadmap

```
Phase 0 classification + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter reverse-engineering
  → Phase 2 auth three gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merge report + insert_assets batch-insert all discovered services and endpoint assets; no discovered asset may be omitted when inserting
```

Check off in order; **do not enter the next Phase until the previous one is complete**.

1. [ ] **Phase 0**: probe SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy script → **immediately** harvest → `wc -l` validate → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor windowing + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: auth three gates → `config.json` → [Phase 2](#phase-2--auth-three-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-restoration)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Produces `forms.txt`, `links.txt`, `api_inline.txt`. If SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) · [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: 404 means modify harvest and retry; do not manually curl chunks one by one
- `api_static.txt` too small → relax the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — Parameter Reverse-Engineering

Paths come from Phase 1; parameter fields require separate recon. grep rules in [Tool and Output Constraints](#tool-and-output-constraints).

**Definition of Done**: for important interfaces you can answer — field name, transport location, type inference, required or not, sample values, confidence.

#### 1b.0 — Transport Forms

| Form | Where parameters live | Static-first look |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path params | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped into `sign`/`data` | Hook encryption function inputs (reference section D) |

Output: annotate each interface with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor Windowing

Using known paths as anchors, widen the window to find packaging objects:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Packaging layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| unified request | interceptor-injected global fields |
| OpenAPI client | generated method signatures |
| React Query / SWR | hook second argument |
| Vue composable | composable inputs |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Routing | `:id` / `?tab=` |
| Interceptors | global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

Use the DevTools call stack from `fetch`/`XHR.send` upward to trace packaging functions.

#### 1b.3 — Packaging Three Questions (≠ Phase 2 auth three gates)

| Question | What to answer |
|---|---|
| **Assembly** | where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) additionally reads globally-injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff to Phase 3

Candidate fields come from the static/binding layer; **required/optional/conditional dependencies** need the Phase 3 parameter matrix + diff + Phase 5 error back-inference.

---

## Phase 2 — Auth Three Gates

grep inside `OUTDIR/js/` (with `head`), write to `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is logged-in determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage key names as credentials — they must be confirmed from the chunk/request chain.

**Exit = Gate B**: conclusions land in `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (optional)

Use `preload.js` inside OUTDIR to confirm session key names, Authorization, and nested API URLs:

| Configuration | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Each coverage round exports: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Must have passed Gate B; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--violations-are-out-of-scope) · the credential-less mock strategy.

`config.json` sets `"runtimeMode": "depth" | "coverage" | "both"` (template in reference).

### Hooks and Stubs (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 precise | auth/permission/bootstrap stubs | pass first-screen auth |
| L2 negative correction | all JSON responses | not-logged-in codes → success |
| L3 fallback | `/api` etc. not hit by L1 | empty success body to prop open the UI |

- **depth**: fake auth + `forward` to patch business codes + `stubs`; iterate `routes` (hash/history); produce `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verification: `window.__API_RECON_PRELOAD__` exists; business paths don't return to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Coverage Dynamic Enumeration (mandatory)

1. Main navigation/sidebar — click each item, wait 1–3s for network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — first-row view/edit/detail
4. Toolbar — export, filter, create (**avoid irreversible deletes**)
5. Each module entry — merge APIs/routes
6. SPA — controlled `pushState` for paths not covered by `routes.txt` (forbidden for MPA)

**Parameter trigger matrix** (mandatory): record once per operation type per module, **diff multiple samples**:

| Operation | Usually-extra parameters |
|---|---|
| List first screen | pagination + default filters |
| Click search | keyword, filter |
| Advanced filters | more optional |
| Create/edit | complete entity |
| Batch/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stubs, outbound body/headers are still real** — requests are authoritative. Record → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: first 3a depth, then 3b coverage

---

## Phase 4 — Permission Tree Restoration

**Trigger**: module pages blank / only bootstrap (e.g. locale) per route → content gate not passed.

| Symptom | Meaning |
|---|---|
| Shell entry succeeded | render gate + interceptor gate passed |
| Sidebar missing items / clicking blank | stub shape or permission codes incomplete |
| Same minimal API per route | `v-if permission` not passed |
| `routes.txt` far fewer than bundle | must backfill from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub check: outer `response_code` consistent with the interceptor gate; flat codes aligned with the tree; `routes` covers all links of `route_map`.

After updating `config.json`, **rerun Phase 3**. Large SPAs can tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and Report

### Output Table

| File | Phase | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime configuration |
| `runtime_api.json` | 3a | depth detailed recording (incl. WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multi-sample, click logs, detail |
| `route_map.json` etc. | 4 | Permission-tree intermediate files (if executed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Write all services and endpoint assets into the asset library |

### 5b — Parameter Merge

Diff from `param_samples.json`; **no generic merge script**. Confidence rules in reference J7 (high/medium/low/to-trigger).

### 5c — Error Back-Inference

Within authorization, incomplete requests may be sent to read 400s (**parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Mind `data` wrapping, `variables`, and `bizData` before encryption.

The report must note: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a `CHANGES.md` summary relative to the reference scripts.

Suggested `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

More fields and grep recipes in [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy-load methods are the same
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch is recordable; RSC/Server Actions are not fully enumerable
- **Blind spots**: JSVMP, WASM, HMAC/mTLS strong verification → static + mark limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM packaging → "to-trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, Hooks, parameter reverse-engineering section J, site_map template: **[reference.md](reference.md)**
- Reference script paths in the [Scripts and Gates](#scripts-and-gates) table
