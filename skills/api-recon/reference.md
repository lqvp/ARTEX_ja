# api-recon — Reference Manual

Grep recipes, `config.json` templates, and troubleshooting. All greps run against the `js/` directory. When the bundle is a single line, you can first run `js-beautify` or `sed 's/}/}\n/g'`; usually raw grep with context windows is enough.

## Script Notes

All files inside `scripts/` are **reference templates** and must be adapted to the target site before execution. Typical change points:

| Script | Common items to adapt |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub matching rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-domain filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming patterns |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | unified entry for all site-specific parameters above |

Adapted files are best placed in the task working directory (e.g. `recon/`), and the report should note the specific changes relative to the reference scripts.

---

## A. Reverse-Engineering the Three Gates

### A1. Render Gate — "How is logged-in determined?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Find the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` to determine the **storage key**, **container** (Cookie vs localStorage), and **encoding**:

| Encoding | How to forge in config |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | unsigned/`alg:none` JWT, or sign with a key inside the bundle |
| Encrypted (SM2/AES/RSA) | find hardcoded keys; forge only when a decodable blob is enough for the render gate; otherwise fall back to static |

→ Write to `cookies` / `localStorage`.

### A2. Interceptor Gate — "What triggers redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: **field name**, **success value** (usually `0` or `200`), and **failure values that trigger redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write to `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — "Where do menus/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two-layer data** (common in enterprise backends):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` inside bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` inside bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree restoration** (large sites, sidebar/submodules still blank): see **Section I**.

---

## B. config.json Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field descriptions:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards real requests and rewrites code fields; `false` is a fully offline stub
- `mockTier`: preload enable tiers in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging menus the harness auto-appends `<a href>`
- `captureResponses` / `recordWs` are only effective in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid `networkidle2` hangs
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` also work

### B1. Dual Stub Template (role_permissions + permissions/all)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

Outer field names (`response_code` / `code` / `data`) must be consistent with the A2 interceptor gate; `permissions` must cover all leaf codes in the tree.

---

## C. coverage Mode: preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before CDP injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verification: `window.__API_RECON_PRELOAD__ === true` and pathname is stable.

Export recording results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook Capabilities

Built-in browser Hook capabilities and coverage of preload (coverage) and runtime_harvest (depth):

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | discover headers like Authorization | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | confirm session key names | ⚠️ optional `observe.storageReads/cookieReads` |
| Vue route acquisition | complete frontendRoutes | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Vue route-guard neutralization / login-redirect blocking | prop open module-triggered APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| React route acquisition | supplement routes | ⚠️ static + clicks; no dedicated Hook |
| Page navigation blocking (login path) | stay on page for analysis | ⚠️ only block the login path, avoid blocking business navigation |
| Hook encryption library (CryptoJS/SM etc.) | encrypted parameters → plaintext API body | ❌ must manually Hook encryption function inputs; conclusions go to config |
| Anti-debug bypass | otherwise runtime cannot record APIs | ❌ must be handled manually; static still works |

---

## E. Endpoint Extraction Regex (when static yields too little)

Relax `extract_endpoints` in `harvest_static.py`, or manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Fix |
|---|---|
| Very few static APIs | endpoint dialect mismatch → relax regex (section D) |
| chunk count ≪ manifest | CSS-only or undeployed chunks; 404 already retried |
| runtime still shows login page | render gate error → recheck A1: key name, container, encoding, domain |
| shell entered but modules blank | content gate → forge menus (A3); `routes` path may be wrong |
| only bootstrap/locale per route | incomplete permission codes → section I permission-tree restoration; check `role_permissions` + `permissions/all` dual stubs |
| sidebar has items but subpages blank | tree missing intermediate nodes or code inconsistent with `userRouteAuth` |
| every API redirects to login | interceptor gate → confirm `neutralize`; nested fields need extended walk logic |
| WS frames are 0 | subscribe requires user interaction; lengthen `perRouteMs` |
| empty response bodies | only real responses with `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| still redirects to login despite many mocks | Hook too late or missing `location.href` setter → document-start + preload |
| lists all empty | L3 empty arrays are normal; keep clicking tabs/settings/details |
| mistaking Redux actions for routes | filter internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload not document-start → change injection timing; or when `neutralizeVueRouter: false`, manually clear guards |
| URL in response but not in log | enable `extractUrlsFromResponse`; or manually extract from `__API_RECON_DETAIL__` |
| don't know the Authorization header name | enable `observe.xhrHeaders` or inspect request headers in DevTools |
| runtime extremely slow / timeout | change `waitUntil: domcontentloaded`; lower `routeTimeout`; don't use `networkidle2` |
| proxy connection failure | check `proxy` / environment variables; Puppeteer and curl proxy ports must match |

---

## G. Hardened Targets

When the server validates sessions step by step (unforgeable signed cookies, server-side rendered menus that can't be stubbed), runtime will stall at the shell. Expected behavior:

- **Static is sufficient for endpoint enumeration** — module paths are in the code
- If authorization permits, run the same harness with a **real session**: `forward: true`, no neutralization needed, capturing real methods/params/responses

---

## H. Single-Task Checklist

1. Confirm the authorization scope
2. **Read** `scripts/harvest_static.py` → adapt to the target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor windowing + binding layer → `param_candidates.json` (section J)
4. Reverse-engineer A1/A2/A3 → write site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before executing
6. `runtimeMode=depth`: `npm install` → run the adjusted harvest script
7. `runtimeMode=coverage/both`: document-start inject the adjusted preload → browser MCP dynamic enumeration + **parameter trigger matrix**
8. Modules not rendering → **section I permission-tree restoration** → patch stubs → rerun
9. Multi-sample parameter diff + error back-inference → `params_merged.json`
10. Merge → `site_map.json` + `api_merged.txt`, honestly noting coverage, gaps, and script change points

---

## I. Permission Tree Restoration (Phase 4 Deepening)

Use when forging simple `menus: [{ path, show: true }]` doesn't work and submodules still don't mount.

### I1. Locate the Auth Module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: **permission API path**, **response field names**, **consumer chunk file names**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If `[!] no routeMap pattern found`: relax the regex in `extract_route_map.py`, or manually grep:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build Permission Tree + Stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve aliases → real paths
3. Infer parents by code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, auto-write into `config.json`'s `stubs` and extended `routes`

**Adapt to the target** (at the top of the script):
- `DEFAULT_ROOTS`: top-level module code list
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with non-prefix relationships

### I4. Validate Stub Consistency

```bash
# permissions count should ≈ number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover all links in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and Compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare runtime_api.json counts before/after forging; check whether module APIs appear for /attack, /asset etc.
```

| Before forge | After forge (success) |
|---|---|
| Same 3–5 bootstrap entries per route | Different routes trigger different module APIs |
| Only `/api/locale/language` | `/api/web/...` module endpoints appear |
| `routes.txt` has single-digit routes | `routes` 80–110+ from route_map |

### I6. When It Still Fails

- **coverage mode**: click sidebar + Tabs; permission gating may only request after interaction
- **stub fields**: compare real API (curl + real session) with stub nesting
- **additional guards**: grep `hasPermission|checkRole|func.` etc. for button-level checks, extend `role_permissions.permissions`
- **static fallback**: module API paths are still in `api_static.txt`; runtime only supplements METHOD/body; parameters keep `param_candidates.json` + recorded samples

---

## J. Parameter Reverse-Engineering (Phase 1b / 5b / 5c)

**Methodology, not a generic script.** Use regex to find paths; use anchor windowing + UI binding chain + multi-sample diff + error back-inference to find parameters.

### J1. Anchor Windowing — Find Packaging Objects from Paths

```bash
# anchor on known paths from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Packaging Layer and Transport Forms

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path params
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation Gate — Required / Format / Enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding Layer — Form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime supplement: DevTools → Network → request → **Initiator** (call stack) to trace packaging functions upward from `fetch`/`send`.

### J5. Encrypted Parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields on ciphertext** — Hook the encryption function **inputs** and record the plaintext payload before encryption; conclusions go to `config.json` / `param_candidates.json`.

### J6. Parameter Trigger Matrix (mandatory in Phase 3)

For each module, record once per operation and diff request body/query:

| Operation | Focus |
|---|---|
| List first screen | pagination defaults |
| Search | keyword, filters |
| Advanced filters | optional fields |
| Create/edit | complete entity |
| Batch/export | `ids[]`, `exportType` |
| Sort/paging | `sortField`, `order` |

Produce `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence Rules

| Confidence | Condition |
|---|---|
| **High** | static callsite + runtime ≥2 consistent samples |
| **Medium** | static only, or only 1 runtime sample |
| **Low** | response/error back-inference, not secondarily verified |
| **To trigger** | statically known fields, UI/permissions not yet reached |

### J8. Scenario Quick Recipes

| Scenario | Order |
|---|---|
| REST list page | J1 packaging object → J6 four diffs → J3 rules |
| Create/edit form | J3 Form name → J4 submit chain → runtime submit + deliberately leave blank to see 400 |
| GraphQL | J2 variables declarations → runtime record variables per operation |
| Encrypted body | J5 Hook inputs → pre-encryption fields are the real params |

### J9. Mapping to api-recon Phases

| api-recon | Parameter recon |
|---|---|
| Phase 1 static | J1 anchor windowing |
| Phase 2 A2 interceptor | globally-injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | different module forms differ → full permissions trigger all fields |
| Phase 5 merge | `params_merged.json` + confidence; never determine required from a single sample |

### J10. Troubleshooting

| Symptom | Fix |
|---|---|
| static has field names but runtime never shows them | mark "to trigger"; supplement permission tree / click advanced filters / linked selects for each option |
| same path with different body shapes | normal — record separately per `action`, don't force-merge schemas |
| stub response fake but want to see params | **look at outbound request** body/headers, don't back-infer from stub responses |
| 400 reports nested field | mind outer wrapping `data`/`bizData`/`variables` |
| GraphQL only shows operation names | expand the `variables` JSON; statically find `$var: Type` |
