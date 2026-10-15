#!/usr/bin/env node
/**
 * check-catalog.mjs — namespace-aware i18n catalog audit (Task 5).
 *
 * Why namespace-relative: next-intl's `useTranslations("nav")` scopes `t("items.dashboard")`
 * to `messages.nav.items.dashboard` (Ruling R6). A root-based resolver therefore reports false
 * missing/dead findings. This script attributes every translator call site to the enclosing
 * `useTranslations("<ns>")` / `getTranslations("<ns>")` binding in the same file, then resolves
 * `"<ns>.<key>"`.
 *
 * Audits produced in one pass over `src/`:
 *   1. missing   — referenced key absent, or resolves to a non-string node (object/array).
 *   2. dead      — string leaf present in the catalog but never referenced.
 *   3. unscoped  — translator call with no resolvable namespace binding.
 *   4. english-chrome — user-visible English strings outside components/ui/**, lib/mock/**, and
 *      outside the R10-dead `nav-documents.tsx` (see EXCLUDE_FROM_ENGLISH_SCAN).
 *      Task 13e then applied the deliberate R9 technical-identifier decision recorded in
 *      docs/superpowers/i18n-migration-ledger.md ("## Task 13e — R9 technical-identifier
 *      decision"): 18 sites are pre-existing English technical identifiers / brands / example
 *      values (command names, URLs, a model ID, an API-key format example, HTTP header names,
 *      domain nouns) that ruling R9 explicitly exempts from localization. Cataloguing URLs,
 *      `npx` command lines, model IDs, or key-format examples would be wrong, so they stay
 *      English. They are skipped by ENGLISH_COPY_EXEMPT_SITES, which is site-scoped exact
 *      `{file, text}` equality on BOTH fields — deliberately NOT word-scoped (words like
 *      `host`, `App`, `IP` are genuine chrome in other contexts, so a word-level exemption
 *      would hide real findings elsewhere: checker-tuning, forbidden by R13) and deliberately
 *      not line-number-based (line numbers rot on every edit). Fail-open: only an exact
 *      double match drops a finding; every other finding reports exactly as before.
 *
 * Exit code is non-zero only for `missing` (or for english chrome when
 * `--english-chrome-strict` is passed). `dead`, `unscoped`, and the ambiguity/destructuring
 * notes are warnings: they surface real debt without blocking later P1 tasks. Task 13 flips
 * english chrome to a hard gate.
 *
 * Translator call shapes handled: `t("k")`, `t.rich("k")`, `t.markup("k")`, `t.raw("k")`,
 * `t.has("k")` (use-intl 4.14.9 createBaseTranslator), under a plain binding
 * `const t = useTranslations("ns")` (also `getTranslations`, also `await`), a no-arg root
 * binding `const t = useTranslations()`, or a `t`/`tc` default name in a next-intl-aware file.
 *
 * Known limitations (declared, not silent):
 *   - Files that never reference next-intl are skipped by the `i18nAware` gate, so a translator
 *     passed in as a prop from another file cannot be attributed and is not reported. This is
 *     intentional: dropping the gate would turn every unrelated `t(` into `unscoped` noise.
 *   - A fake call written inside a template-literal's literal text (`` `t("k")` ``) still
 *     inflates the reference set; single/double-quoted strings are excluded, templates are not.
 *   - Destructured bindings (`const { t: label } = useTranslations("ns")`) are treated as an
 *     unsupported shape and their call sites are reported as `unscoped`, never silently resolved.
 *   - `new Error("...")` strings are exempt from english-chrome. This file deferred that
 *     policy question to Task 13, and Task 13 resolved it: an internal
 *     `throw new Error("...")` is category-2 control flow, never user-visible copy —
 *     consistent with Task 11's ruling that `api.ts:135`
 *     `throw new Error("未授权")` is category 2.
 *
 * Usage:
 *   node scripts/check-catalog.mjs
 *   node scripts/check-catalog.mjs --source <dir> --catalog <file>
 *   node scripts/check-catalog.mjs --english-chrome-strict
 *   node scripts/check-catalog.mjs --self-test
 */

import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

// components/ui is vendored/shadcn and excluded from copy scanning; lib/mock is fixture
// data excluded only from the english-chrome scan.
const EXCLUDE_FROM_KEY_SCAN = ["components/ui"];
// isExcluded() compares these as PLAIN STRINGS (no regex), with three branches:
//   relPath === segment | relPath.startsWith(segment + "/") | relPath.includes("/" + segment + "/")
// so a segment must be either the whole src-relative path or a "/"-terminated directory
// prefix. The parentheses in "(main)" are literal characters, not a regex group.
// Ruling R10: `nav-documents.tsx` is dead code (commented-out callers) and is out of scope
// for copy, so the whole file is excluded — from the english scan ONLY, so its keys still
// feed the `dead` audit exactly as before.
const EXCLUDE_FROM_ENGLISH_SCAN = ["components/ui", "lib/mock", "src/app/(main)/_components/sidebar/nav-documents.tsx"];
const DEFAULT_TRANSLATOR_NAMES = ["t", "tc"];
const SIDEBAR_ITEMS_PATH = join(projectRoot, "src/navigation/sidebar/sidebar-items.ts");

// Ruling R9 + Task 5 addendum exemptions: domain nouns, brand names, and technical
// identifiers that stay verbatim in the Japanese UI.
const EXEMPT_WORDS = new Set(
  [
    // domain nouns
    "finding",
    "asset",
    "intercept",
    "traffic",
    "session",
    "retest",
    "agent",
    "skill",
    "tool",
    "mcp",
    "llm",
    "rce",
    "poc",
    // brand / product names
    "artex",
    "anthropic",
    "tavily",
    "deepseek",
    "geist",
    // technical identifiers shown verbatim
    "http",
    "get",
    "post",
    "put",
    "patch",
    "delete",
    "head",
    "options",
    "key",
    "token",
    "model",
    "profile",
    "worker",
    "method",
    "tools",
    "json",
    "api",
    "url",
    "id",
  ].map((word) => word.toLowerCase()),
);
const EXEMPT_PHRASES = ["api key"];

// Task 13e — the deliberate R9 technical-identifier decision, recorded in
// docs/superpowers/i18n-migration-ledger.md ("## Task 13e — R9 technical-identifier decision").
// The 18 entries below are pre-existing English technical identifiers / brands / example values
// that ruling R9 explicitly exempts from localization; cataloguing them (URLs, `npx` command
// lines, model IDs, API-key format examples) would be wrong, so they stay English. The filter
// is SITE-scoped `{file, text}` and deliberately NOT word-scoped: `host`, `App`, `IP`,
// `filesystem`, `npx` etc. are genuine user-visible chrome in other contexts (a real `Host`
// column header, a real `App` badge), so a word-level exemption (adding them to EXEMPT_WORDS)
// would silently hide findings elsewhere — checker-tuning, forbidden by R13. Matching is EXACT
// equality on BOTH fields, never substring/prefix, and never line-number-based (line numbers
// rot on every edit), so a sibling file or a different string in the same file never gets
// swept in. Fail-open: a finding is dropped only when both fields match; everything else
// reports exactly as before.
const ENGLISH_COPY_EXEMPT_SITES = [
  // system/mcp — command names, npx args, and a URL example (R9: technical identifiers verbatim).
  { file: "src/app/(main)/system/mcp/page.tsx", text: "filesystem", why: "R9 command name (MCP server package)" },
  { file: "src/app/(main)/system/mcp/page.tsx", text: "npx", why: "R9 command name" },
  {
    file: "src/app/(main)/system/mcp/page.tsx",
    text: "-y @modelcontextprotocol/server-filesystem /data",
    why: "R9 npx args",
  },
  { file: "src/app/(main)/system/mcp/page.tsx", text: "https://mcp.example.com/mcp", why: "R9 URL example" },
  // function/traffic — an example value plus two HTTP header names (R9: technical identifiers verbatim).
  { file: "src/app/(main)/function/traffic/page.tsx", text: "host…", why: "R9 example value" },
  { file: "src/app/(main)/function/traffic/page.tsx", text: "host", why: "R9 HTTP header name" },
  { file: "src/app/(main)/function/traffic/page.tsx", text: "content-type", why: "R9 HTTP header name" },
  // system/llm — a model ID and URL examples (R9: technical identifiers verbatim).
  { file: "src/app/(main)/system/llm/page.tsx", text: "claude-opus-4-8", why: "R9 model ID" },
  { file: "src/app/(main)/system/llm/page.tsx", text: "https://api.openai.com/v1", why: "R9 provider base URL" },
  {
    file: "src/app/(main)/system/llm/page.tsx",
    text: "socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080",
    why: "R9 proxy URL examples",
  },
  // function/sync — a brand name and an API-key format example.
  { file: "src/app/(main)/function/sync/page.tsx", text: "ScopeSentry", why: "R9 brand/product name" },
  { file: "src/app/(main)/function/sync/page.tsx", text: "ssk_...", why: "R9 API-key format example" },
  // coverage-graph-tab — domain nouns kept Latin inside Japanese copy (R9 + R15).
  {
    file: "src/app/(main)/function/tasks/detail/_tabs/coverage-graph-tab.tsx",
    text: "IP",
    why: "R9+R15 domain noun",
  },
  {
    file: "src/app/(main)/function/tasks/detail/_tabs/coverage-graph-tab.tsx",
    text: "App",
    why: "R9+R15 domain noun",
  },
  // system/skills — example values (skill name, license identifiers).
  { file: "src/app/(main)/system/skills/page.tsx", text: "sqli-deepdive", why: "R9 example skill name" },
  { file: "src/app/(main)/system/skills/page.tsx", text: "MIT / Proprietary", why: "R9 license identifiers" },
  // system/notify — URL example; side-question-workspace — command token.
  { file: "src/app/(main)/system/notify/page.tsx", text: "https://artex.example.com", why: "R9 URL example" },
  { file: "src/components/side-question-workspace.tsx", text: "/btw", why: "R9 command token" },
];

// Fail-open R9 filter (Task 13e): an english-chrome finding is dropped only when BOTH its
// relPath and its whitespace-normalized text exactly equal an ENGLISH_COPY_EXEMPT_SITES entry.
// No substring, prefix, or line-number matching — anything that does not match exactly keeps
// reporting, so the filter can only ever under-suppress, never over-suppress (R13).
function isEnglishCopyExemptSite(relPath, text) {
  return ENGLISH_COPY_EXEMPT_SITES.some((entry) => entry.file === relPath && entry.text === text);
}

const ENGLISH_PROP_NAMES =
  "placeholder|title|aria-label|alt|heading|label|description|tooltip|message|emptyMessage|confirmText|submitLabel";

const TRANSLATOR_METHODS = "rich|markup|raw|has";

function parseArgs(argv) {
  let source = join(projectRoot, "src");
  let catalog = join(projectRoot, "src/messages/ja.json");
  let englishStrict = false;
  let selfTest = false;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === "--source") {
      source = resolve(process.cwd(), argv[i + 1]);
      i += 1;
    } else if (arg === "--catalog") {
      catalog = resolve(process.cwd(), argv[i + 1]);
      i += 1;
    } else if (arg === "--english-chrome-strict") {
      englishStrict = true;
    } else if (arg === "--self-test") {
      selfTest = true;
    } else {
      console.error(`check-catalog: unknown argument "${arg}"`);
      process.exit(2);
    }
  }
  return { source, catalog, englishStrict, selfTest };
}

// Replaces comment characters with spaces while preserving length and newlines, so line
// numbers stay accurate and example calls inside comments are not mistaken for real ones.
function stripComments(text) {
  let out = "";
  let i = 0;
  const n = text.length;
  let state = "code";
  while (i < n) {
    const ch = text[i];
    const next = i + 1 < n ? text[i + 1] : "";
    if (state === "code") {
      if (ch === "/" && next === "/") {
        state = "line";
        out += "  ";
        i += 2;
        continue;
      }
      if (ch === "/" && next === "*") {
        state = "block";
        out += "  ";
        i += 2;
        continue;
      }
      if (ch === "'") state = "sq";
      else if (ch === '"') state = "dq";
      else if (ch === "`") state = "tpl";
      out += ch;
      i += 1;
      continue;
    }
    if (state === "line") {
      if (ch === "\n") {
        state = "code";
        out += ch;
      } else {
        out += " ";
      }
      i += 1;
      continue;
    }
    if (state === "block") {
      if (ch === "*" && next === "/") {
        state = "code";
        out += "  ";
        i += 2;
        continue;
      }
      out += ch === "\n" ? "\n" : " ";
      i += 1;
      continue;
    }
    // Inside a string literal: preserve the content verbatim and honour escapes.
    if (ch === "\\") {
      out += ch;
      if (next) {
        out += next;
        i += 2;
      } else {
        i += 1;
      }
      continue;
    }
    if ((state === "sq" && ch === "'") || (state === "dq" && ch === '"') || (state === "tpl" && ch === "`")) {
      state = "code";
    }
    out += ch;
    i += 1;
  }
  return out;
}

// Blanks the *contents* of single/double-quoted string literals (quotes kept) while preserving
// length, so a call that only exists inside a string (`const s = 't("k")'`) is not harvested as
// a real reference. Run after stripComments so quotes inside comments are already gone.
function blankQuotedStrings(text) {
  let out = "";
  let i = 0;
  const n = text.length;
  let quote = "";
  while (i < n) {
    const ch = text[i];
    if (!quote) {
      if (ch === '"' || ch === "'") quote = ch;
      out += ch;
      i += 1;
      continue;
    }
    if (ch === "\\") {
      out += "  ";
      i += 2;
      continue;
    }
    if (ch === quote) {
      quote = "";
      out += ch;
      i += 1;
      continue;
    }
    out += ch === "\n" ? "\n" : " ";
    i += 1;
  }
  return out;
}

function walk(dir, acc) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, acc);
    else if (/\.tsx?$/.test(entry)) acc.push(full);
  }
  return acc;
}

function toPosix(path) {
  return path.split(sep).join("/");
}

function isExcluded(relPath, segments) {
  return segments.some(
    (segment) => relPath === segment || relPath.startsWith(`${segment}/`) || relPath.includes(`/${segment}/`),
  );
}

function lineOf(text, index) {
  return text.slice(0, index).split("\n").length;
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function collectLeafPaths(node, prefix, out) {
  if (typeof node === "string") {
    if (prefix) out.push(prefix);
    return;
  }
  if (node === null || typeof node !== "object" || Array.isArray(node)) return;
  for (const [key, value] of Object.entries(node)) {
    collectLeafPaths(value, prefix ? `${prefix}.${key}` : key, out);
  }
}

function resolveKey(catalog, namespace, key) {
  const path = namespace ? `${namespace}.${key}` : key;
  let node = catalog;
  for (const part of path.split(".")) {
    if (node === null || typeof node !== "object") return undefined;
    node = node[part];
  }
  return node;
}

// Plain bindings only: `const t = useTranslations("ns")`, `await getTranslations("ns")`, or the
// no-argument root form. Returns Map<name, { namespaces: string[], lines: number[] }> so an
// ambiguous name (one name, two namespaces) can be detected instead of last-one-wins.
function extractScopes(stripped) {
  const scopes = new Map();
  const add = (name, namespace, line) => {
    if (!scopes.has(name)) scopes.set(name, { namespaces: [], lines: [] });
    const entry = scopes.get(name);
    entry.namespaces.push(namespace);
    entry.lines.push(line);
  };
  const withNamespace =
    /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:await\s+)?(?:use|get)Translations\s*\(\s*(["'])([^"']*)\2\s*\)/g;
  for (const match of stripped.matchAll(withNamespace)) add(match[1], match[3], lineOf(stripped, match.index));
  const withoutNamespace =
    /(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:await\s+)?(?:use|get)Translations\s*\(\s*\)/g;
  for (const match of stripped.matchAll(withoutNamespace)) {
    if (!scopes.has(match[1])) add(match[1], "", lineOf(stripped, match.index));
  }
  return scopes;
}

// Destructured bindings are not a supported shape at runtime (the translator is a function, not
// an object with a `t` property), so they are surfaced as `unscoped` rather than resolved. This
// keeps the pre-existing plain `const { t } = useTranslations(...)` tripwire working and extends
// it to the silent aliased twin `const { t: label } = ...`.
function extractDestructuredBindings(stripped) {
  const names = new Set();
  const entries = [];
  const re = /(?:const|let|var)\s*\{([^}]*)\}\s*=\s*(?:await\s+)?(?:use|get)Translations\s*\(\s*([^)]*)\)/g;
  for (const match of stripped.matchAll(re)) {
    const line = lineOf(stripped, match.index);
    const namespace = /^\s*(["'])([^"']*)\1\s*$/.exec(match[2])?.[2] ?? "";
    for (const rawProperty of match[1].split(",")) {
      const property = rawProperty.trim();
      if (!property) continue;
      const withoutDefault = property.split("=")[0].trim();
      const parts = withoutDefault.split(":");
      const boundName = (parts.length > 1 ? parts[1] : parts[0]).trim();
      if (!/^[A-Za-z_$][\w$]*$/.test(boundName)) continue;
      names.add(boundName);
      entries.push({ name: boundName, line, namespace });
    }
  }
  return { names, entries };
}

function extractTranslatorFindings(
  stripped,
  codeOnly,
  relPath,
  scopes,
  destructuredNames,
  i18nAware,
  catalog,
  findings,
) {
  const names = new Set(scopes.keys());
  for (const name of destructuredNames) names.add(name);
  if (i18nAware) for (const name of DEFAULT_TRANSLATOR_NAMES) names.add(name);

  for (const name of names) {
    const entry = scopes.get(name);
    const namespaces = entry ? [...new Set(entry.namespaces)] : [];
    const ambiguous = namespaces.length > 1;
    if (ambiguous) findings.ambiguous.push({ relPath, line: entry.lines[0], name, namespaces });
    const hasBinding = Boolean(entry);
    const namespace = hasBinding && !ambiguous ? namespaces[0] : undefined;
    let reason = "no namespace binding";
    if (destructuredNames.has(name)) reason = "destructuring binding";
    const base = `(?<![\\w$.])${escapeRegExp(name)}\\s*(?:\\.(?:${TRANSLATOR_METHODS}))?\\s*\\(`;

    const literalRe = new RegExp(`${base}\\s*(["'])([^"'\\\\]*)\\1\\s*[,)]`, "gd");
    const literalIndices = new Set();
    for (const match of codeOnly.matchAll(literalRe)) {
      literalIndices.add(match.index);
      const keySpan = match.indices[2];
      const key = stripped.slice(keySpan[0], keySpan[1]);
      const line = lineOf(stripped, match.index);
      if (ambiguous) {
        const resolved = namespaces.filter((candidate) => typeof resolveKey(catalog, candidate, key) === "string");
        findings.ambiguousCalls.push({ relPath, line, name, key, candidates: namespaces, resolved });
        if (resolved.length === 0) {
          findings.missing.push({ relPath, line, key, namespace: undefined, candidates: namespaces });
        } else {
          for (const candidate of resolved) findings.referenced.add(`${candidate}.${key}`);
        }
        continue;
      }
      if (!hasBinding) {
        findings.unscoped.push({ relPath, line, name, key, reason });
        continue;
      }
      const value = resolveKey(catalog, namespace, key);
      const path = namespace ? `${namespace}.${key}` : key;
      if (typeof value !== "string") findings.missing.push({ relPath, line, key, namespace, path });
      else findings.referenced.add(path);
    }

    const callRe = new RegExp(`${base}\\s*([^)\\n]{0,48})`, "g");
    for (const match of codeOnly.matchAll(callRe)) {
      if (literalIndices.has(match.index)) continue;
      findings.dynamic.push({
        relPath,
        line: lineOf(stripped, match.index),
        name,
        snippet: match[1].replace(/\s+/g, " ").trim(),
      });
    }
  }
}

// Ruling 25 + Task 5 addendum + Ruling 32: `t(item.titleKey)` keys live in data, so scan
// `titleKey`/`labelKey` string literals generically across src (not one hardcoded path). The
// literal is namespace-agnostic: resolve it fully-qualified at the catalog root first (handles
// `status.ts`'s `"status.intent.open"`), then under each top-level namespace (handles the
// sidebar's relative `"items.dashboard"` consumed under `useTranslations("nav")`). Every
// resolving path is referenced (Set entries dedup); only a literal that resolves none is missing.
function extractDataKeys(stripped, relPath, catalog, findings) {
  const re = /\b(?:titleKey|labelKey)\s*:\s*(["'])([^"'\\]*)\1/g;
  const namespaces = Object.keys(catalog);
  for (const match of stripped.matchAll(re)) {
    const key = match[2];
    const line = lineOf(stripped, match.index);
    const resolved = [];
    if (typeof resolveKey(catalog, "", key) === "string") resolved.push(key);
    for (const namespace of namespaces) {
      if (typeof resolveKey(catalog, namespace, key) === "string") resolved.push(`${namespace}.${key}`);
    }
    if (resolved.length === 0) {
      // No namespace was actually expected, so do not assert one: "(any)" is reported as-is.
      findings.missing.push({ relPath, line, key, namespace: "(any)", path: key });
    } else {
      for (const path of resolved) findings.referenced.add(path);
    }
  }
}

function hasEnglishCopy(raw) {
  let value = raw;
  for (const phrase of EXEMPT_PHRASES) value = value.replace(new RegExp(escapeRegExp(phrase), "gi"), " ");
  const words = value.match(/[A-Za-z][A-Za-z'’-]*/g) ?? [];
  return words.some((word) => word.length >= 2 && !EXEMPT_WORDS.has(word.toLowerCase()));
}

// JSX text captured between tags can actually be operator/type syntax (`=> f(x)`,
// `Record<string, T>`); prose never contains these punctuation characters.
function isProseText(raw) {
  const text = raw.trim();
  if (text.length < 2) return false;
  if (/[(){}[\]<>;=]/.test(text)) return false;
  if (text.includes("//") || text.includes("/*") || text.includes("*/")) return false;
  return true;
}

// --- Evidence for "this literal is a catalog KEY, not copy" (Ruling R9) -----------------------
// A `label="..."` style attribute can carry a technical key that the receiving component looks
// up itself (`<Field label="fieldMatchType">` where `Field` renders `{t(label)}`). That is only
// provable from local evidence, so all four conditions below must hold; if any one fails the
// finding is reported exactly as it was before. This is deliberately NOT a blanket `label=`
// exemption: a component that renders the prop verbatim still reports, and element text
// (`<Label>license</Label>`) is a different code path that never reaches this check.

// Index of the `)` matching the `(` at `open`, skipping over string literals. -1 when unbalanced.
function matchBalancedParen(text, open) {
  let depth = 0;
  let quote = "";
  for (let i = open; i < text.length; i += 1) {
    const ch = text[i];
    if (quote) {
      if (ch === "\\") i += 1;
      else if (ch === quote) quote = "";
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") quote = ch;
    else if (ch === "(") depth += 1;
    else if (ch === ")") {
      depth -= 1;
      if (depth === 0) return i;
    }
  }
  return -1;
}

// Index of the `}` matching the `{` at `open`. -1 when unbalanced.
function matchBalancedBrace(text, open) {
  let depth = 0;
  let quote = "";
  for (let i = open; i < text.length; i += 1) {
    const ch = text[i];
    if (quote) {
      if (ch === "\\") i += 1;
      else if (ch === quote) quote = "";
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") quote = ch;
    else if (ch === "{") depth += 1;
    else if (ch === "}") {
      depth -= 1;
      if (depth === 0) return i;
    }
  }
  return -1;
}

// Plain identifier bindings of a destructured parameter list: `({ label, hint?: string })`
// -> ["label", "hint"]. Rest elements and renamed/computed bindings are ignored.
function destructuredPropNames(params) {
  const start = params.indexOf("{");
  if (start < 0) return [];
  const end = matchBalancedBrace(params, start);
  if (end < 0) return [];
  const parts = [];
  let depth = 0;
  let current = "";
  for (let i = start + 1; i < end; i += 1) {
    const ch = params[i];
    if (ch === "{" || ch === "[" || ch === "(") depth += 1;
    else if (ch === "}" || ch === "]" || ch === ")") depth -= 1;
    if (ch === "," && depth === 0) {
      parts.push(current);
      current = "";
      continue;
    }
    current += ch;
  }
  parts.push(current);
  const names = [];
  for (const part of parts) {
    const head = part.split("=")[0].split(":")[0].replace(/\?/g, "").trim();
    if (/^[A-Za-z_$][\w$]*$/.test(head)) names.push(head);
  }
  return names;
}

// ComponentName.prop for every prop of an uppercase-initial component defined in this file
// whose ONLY use inside the component body is as a translator argument (`t(label)`).
// A component that also renders the prop (`{label}`, `title={label}`) fails that test and is
// never recorded, so its call sites keep reporting.
function collectTranslatedProps(stripped) {
  const defs = [];
  const defRe =
    /(?:function\s+([A-Za-z_$][\w$]*)\s*\()|(?:(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?(?:function\s*)?\()/g;
  for (const match of stripped.matchAll(defRe)) {
    const open = match.index + match[0].length - 1;
    const close = matchBalancedParen(stripped, open);
    if (close < 0) continue;
    defs.push({ name: match[1] ?? match[2], start: match.index, paramsStart: open, paramsEnd: close });
  }
  defs.sort((a, b) => a.start - b.start);
  const translatorNames = [...new Set([...DEFAULT_TRANSLATOR_NAMES, ...extractScopes(stripped).keys()])];
  const translatorOpen = `(?:(?:${translatorNames.map(escapeRegExp).join("|")})\\s*(?:\\.(?:rich|markup|raw|has))?\\s*\\()`;
  const out = new Set();
  for (let i = 0; i < defs.length; i += 1) {
    const def = defs[i];
    if (!/^[A-Z]/.test(def.name)) continue;
    const props = destructuredPropNames(stripped.slice(def.paramsStart + 1, def.paramsEnd));
    if (props.length === 0) continue;
    // Body = the balanced block that opens after the parameter list. If the first `{` belongs
    // to a return-type annotation we get the wrong span, but a wrong span can only FAIL to
    // prove translation (the finding is reported), never hide one.
    const bodyOpen = stripped.indexOf("{", def.paramsEnd + 1);
    const nextDef = defs[i + 1]?.start ?? stripped.length;
    const bodyClose = bodyOpen < 0 ? -1 : matchBalancedBrace(stripped, bodyOpen);
    const bodyEnd = bodyClose < 0 ? nextDef : Math.min(bodyClose, nextDef);
    const body = stripped.slice(def.paramsEnd + 1, bodyEnd);
    for (const prop of props) {
      const ident = new RegExp(`(?:^|[^\\w$])${escapeRegExp(prop)}(?![\\w$])`);
      if (!ident.test(body)) continue;
      const rest = body.replace(new RegExp(`${translatorOpen}\\s*${escapeRegExp(prop)}\\s*[,)]`, "g"), " ");
      if (ident.test(rest)) continue;
      out.add(`${def.name}.${prop}`);
    }
  }
  return out;
}

// True only when the attribute sits on `<ComponentName ...` (no `>` in between) whose prop is
// proven key-carrying AND the literal actually exists as a string leaf in the catalog — a value
// that does not resolve is copy that merely looks like a key, and stays reported.
function isCatalogKeyProp(stripped, index, propName, value, translatedProps, catalog) {
  if (!catalog) return false;
  const open = stripped.lastIndexOf("<", index);
  if (open < 0) return false;
  const head = stripped.slice(open + 1, index);
  if (head.includes(">")) return false;
  const owner = /^\s*([A-Za-z_$][\w$]*)/.exec(head)?.[1];
  if (!owner || !/^[A-Z]/.test(owner)) return false;
  if (!translatedProps.has(`${owner}.${propName}`)) return false;
  if (typeof resolveKey(catalog, "", value) === "string") return true;
  return Object.keys(catalog).some((ns) => typeof resolveKey(catalog, ns, value) === "string");
}

function extractEnglishChrome(stripped, relPath, findings, isTsx, catalog) {
  const add = (index, raw) => {
    const text = raw.replace(/\s+/g, " ").trim();
    if (text.length === 0) return;
    // Task 13e R9 exemption: site-scoped exact {file, text} match, fail-open (see
    // ENGLISH_COPY_EXEMPT_SITES). Only the english-chrome category is filtered here;
    // the other five categories are untouched.
    if (isEnglishCopyExemptSite(relPath, text)) return;
    findings.english.push({ relPath, line: lineOf(stripped, index), text });
  };
  const propPatterns = [
    new RegExp(`\\b(${ENGLISH_PROP_NAMES})\\s*=\\s*"([^"]*)"`, "g"),
    new RegExp(`\\b(${ENGLISH_PROP_NAMES})\\s*=\\s*'([^']*)'`, "g"),
  ];
  // `toast.*` messages are user-visible and are scanned. `new Error("...")` is deliberately
  // absent: Task 13 resolved the policy this file had deferred — an internal
  // `throw new Error("...")` is category-2 control flow, never copy (Task 11 ruled the same
  // for `api.ts:135`).
  const copyPatterns = [
    /\btoast\.(?:error|success|info|warning|message|loading)\s*\(\s*"([^"]*)"/g,
    /\btoast\.(?:error|success|info|warning|message|loading)\s*\(\s*'([^']*)'/g,
  ];
  let translatedProps;
  if (isTsx) {
    for (const match of stripped.matchAll(/>([^<>{}]+)</g)) {
      // A `>` that is the second half of `=>` closes nothing: in
      // `onDelete: (id: string) => Promise<void>` the run ` Promise` followed by `<` is type
      // syntax, not a text node. Real JSX text always follows a genuine tag close (`>`/`/>`),
      // so this one-character test drops type annotations and function signatures only.
      if (stripped[match.index - 1] === "=") continue;
      const text = match[1].trim();
      if (isProseText(text) && hasEnglishCopy(text)) add(match.index, text);
    }
  }
  for (const pattern of propPatterns) {
    for (const match of stripped.matchAll(pattern)) {
      const propName = match[1];
      const value = match[2];
      if (!hasEnglishCopy(value)) continue;
      if (translatedProps === undefined) translatedProps = collectTranslatedProps(stripped);
      if (isCatalogKeyProp(stripped, match.index, propName, value, translatedProps, catalog)) continue;
      add(match.index, value);
    }
  }
  for (const pattern of copyPatterns) {
    for (const match of stripped.matchAll(pattern)) {
      if (hasEnglishCopy(match[1])) add(match.index, match[1]);
    }
  }
}

function makeFindings() {
  return {
    missing: [],
    unscoped: [],
    dynamic: [],
    ambiguous: [],
    ambiguousCalls: [],
    destructured: [],
    english: [],
    referenced: new Set(),
  };
}

// Runs the static analysis over a synthetic source string with a supplied catalog. Used by
// `--self-test` to pin attribution behaviour, not just `resolveKey`. Synthetic sources are
// treated as TSX so the english-chrome guards are pinned too. `relPath` defaults to
// "synthetic" so existing checks are unaffected; the Task 13e self-tests pass a real path to
// exercise the site-scoped ENGLISH_COPY_EXEMPT_SITES filter.
function analyzeSource(source, catalog, relPath = "synthetic") {
  const stripped = stripComments(source);
  const codeOnly = blankQuotedStrings(stripped);
  const scopes = extractScopes(stripped);
  const destructured = extractDestructuredBindings(stripped);
  const i18nAware = /(?:use|get)Translations/.test(stripped) || /from\s+["']next-intl["']/.test(stripped);
  const findings = makeFindings();
  findings.destructured.push(...destructured.entries);
  extractTranslatorFindings(stripped, codeOnly, relPath, scopes, destructured.names, i18nAware, catalog, findings);
  extractDataKeys(stripped, relPath, catalog, findings);
  extractEnglishChrome(stripped, relPath, findings, true, catalog);
  return findings;
}

function selfTest(catalog) {
  const miniCatalog = {
    nav: { items: { dashboard: "D" }, other: "O" },
    common: { cancel: "C", save: "S" },
    auth: { newPassword: "NP", title: "T", onlyAuth: "OA", shared: "AS" },
    changePassword: { newPassword: "CNP", shared: "CS" },
    status: { intent: { open: "SO", closed: "SC" } },
  };
  let failed = 0;
  const check = (label, actual, expected) => {
    const ok = JSON.stringify(actual) === JSON.stringify(expected);
    if (ok) {
      console.log(`PASS ${label}`);
    } else {
      failed += 1;
      console.error(`FAIL ${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
    }
  };

  // resolveKey-level checks (namespace-relative, non-string node is not copy).
  const resolveCases = [
    ["nav", "items.dashboard", true],
    ["nav", "nav.items.dashboard", false],
    [null, "items.dashboard", false],
    ["account", "changePassword", true],
    [null, "changePassword", false],
    ["nav", "items", false],
  ];
  for (const [namespace, key, expectString] of resolveCases) {
    const value = resolveKey(catalog, namespace, key);
    const isString = typeof value === "string";
    const label = `resolve(ns=${JSON.stringify(namespace)}, key=${JSON.stringify(key)}) -> ${isString ? "string" : typeof value}`;
    if (isString === expectString) console.log(`PASS ${label}`);
    else {
      failed += 1;
      console.error(`FAIL ${label}`);
    }
  }

  // Attribution-level checks against synthetic sources.
  const plain = analyzeSource('const t = useTranslations("nav");\nt("items.dashboard");\nt("nope");', miniCatalog);
  check("plain binding referenced", [...plain.referenced], ["nav.items.dashboard"]);
  check(
    "plain binding missing",
    plain.missing.map((m) => m.path),
    ["nav.nope"],
  );
  check("plain binding unscoped", plain.unscoped.length, 0);

  const twoBindings = analyzeSource(
    'const t = useTranslations("auth");\nconst tc = useTranslations("common");\nt("newPassword");\ntc("cancel");',
    miniCatalog,
  );
  check("t/tc referenced", [...twoBindings.referenced].sort(), ["auth.newPassword", "common.cancel"]);
  check("t/tc missing", twoBindings.missing.length, 0);

  const aliased = analyzeSource('const { t: label } = useTranslations("common");\nlabel("cancel");', miniCatalog);
  check(
    "aliased destructuring unscoped",
    aliased.unscoped.map((u) => u.name),
    ["label"],
  );
  check("aliased destructuring referenced", aliased.referenced.size, 0);
  check("aliased destructuring reason", aliased.unscoped[0]?.reason, "destructuring binding");

  const subMethods = analyzeSource(
    'const t = useTranslations("nav");\nt.rich("other", {});\nt.has("items.dashboard");\nt.raw("nope");',
    miniCatalog,
  );
  check("sub-method referenced", [...subMethods.referenced].sort(), ["nav.items.dashboard", "nav.other"]);
  check(
    "sub-method missing",
    subMethods.missing.map((m) => m.path),
    ["nav.nope"],
  );

  // Ambiguity must stay loud but must NOT disable missing-key detection: the key is resolved
  // against every candidate namespace. These cases live next to the `unscoped` cases so the two
  // states can never be conflated again.
  const ambiguous = analyzeSource(
    'function A() {\n  const t = useTranslations("auth");\n  return t("newPassword");\n}\nfunction B() {\n  const t = useTranslations("changePassword");\n  return t("newPassword");\n}',
    miniCatalog,
  );
  check("ambiguous recorded", ambiguous.ambiguous.length, 1);
  check("ambiguous call sites", ambiguous.ambiguousCalls.length, 2);
  check("ambiguous resolves in several candidates (referenced once each)", [...ambiguous.referenced].sort(), [
    "auth.newPassword",
    "changePassword.newPassword",
  ]);
  check("ambiguous missing none", ambiguous.missing.length, 0);
  check("ambiguous not unscoped", ambiguous.unscoped.length, 0);

  const ambiguousOne = analyzeSource(
    'function A() {\n  const t = useTranslations("auth");\n  return t("onlyAuth");\n}\nfunction B() {\n  const t = useTranslations("changePassword");\n}',
    miniCatalog,
  );
  check("ambiguous resolves in one candidate", [...ambiguousOne.referenced], ["auth.onlyAuth"]);
  check("ambiguous one-candidate missing none", ambiguousOne.missing.length, 0);

  const ambiguousNone = analyzeSource(
    'function A() {\n  const t = useTranslations("auth");\n  return t("ghost");\n}\nfunction B() {\n  const t = useTranslations("changePassword");\n}',
    miniCatalog,
  );
  check("ambiguous resolves in no candidate -> missing", ambiguousNone.missing.length, 1);
  check("ambiguous missing names candidates", ambiguousNone.missing[0]?.candidates, ["auth", "changePassword"]);
  check("ambiguous missing not referenced", ambiguousNone.referenced.size, 0);

  const stringOnly = analyzeSource('const t = useTranslations("auth");\nconst s = \'t("title")\';', miniCatalog);
  check("string-embedded call excluded", stringOnly.referenced.size, 0);
  check("string-embedded call not missing", stringOnly.missing.length, 0);

  const concatenated = analyzeSource('const t = useTranslations("auth");\nt("new" + "Password");', miniCatalog);
  check("concatenated key is dynamic", concatenated.dynamic.length, 1);
  check("concatenated key not literal", concatenated.referenced.size, 0);

  // Ruling 32: `titleKey`/`labelKey` literals resolve namespace-agnostically — fully-qualified at
  // the catalog root first, else under each top-level namespace. Only a literal that resolves in
  // none of those ways is missing, and it is kept in the same `missing` bucket (so it drives the
  // exit-1 gate in `report`, which keys off `findings.missing.length`).
  const dataKeys = analyzeSource(
    '[{ labelKey: "status.intent.open" }, { titleKey: "items.dashboard" }, { labelKey: "does.not.exist" }]',
    miniCatalog,
  );
  check("data key resolved at root or under a namespace referenced", [...dataKeys.referenced].sort(), [
    "nav.items.dashboard",
    "status.intent.open",
  ]);
  check(
    "data key unresolved missing",
    dataKeys.missing.map((m) => m.path),
    ["does.not.exist"],
  );
  check(
    "data key unresolved uses (any) namespace placeholder",
    dataKeys.missing.map((m) => m.namespace),
    ["(any)"],
  );
  check("data key unresolved not referenced", [...dataKeys.referenced].includes("does.not.exist"), false);

  // Task 13: english-chrome guards. (a) a function type / arrow is not copy, (b) a catalog key
  // handed to a component that translates it is not copy, (c) an internal `new Error` is not
  // copy — while every genuine finding (element text, a prop rendered verbatim) keeps reporting.
  const arrowType = analyzeSource(
    "function Row({ onGo }: { onGo: () => Promise<void> }) {\n  return <button onClick={onGo}>Save</button>;\n}",
    miniCatalog,
  );
  check(
    "english chrome: arrow/type annotation is not copy, real text kept",
    arrowType.english.map((e) => e.text),
    ["Save"],
  );

  const keyProp = analyzeSource(
    'function Field({ label }: { label: string }) {\n  const t = useTranslations("common");\n  return <Label>{t(label)}</Label>;\n}\n<Field label="cancel" />',
    miniCatalog,
  );
  check("english chrome: catalog key through a translating prop is not copy", keyProp.english.length, 0);

  const verbatimProp = analyzeSource(
    'function Card({ label }: { label: string }) {\n  return <div>{label}</div>;\n}\n<Card label="Reset now" />',
    miniCatalog,
  );
  check(
    "english chrome: prop rendered verbatim still reported",
    verbatimProp.english.map((e) => e.text),
    ["Reset now"],
  );

  const elementText = analyzeSource("<Label>license</Label>", miniCatalog);
  check(
    "english chrome: element text still reported",
    elementText.english.map((e) => e.text),
    ["license"],
  );

  const internalError = analyzeSource('function boom() {\n  throw new Error("Missing Store");\n}', miniCatalog);
  check("english chrome: new Error is internal control flow (Task 13)", internalError.english.length, 0);

  // Task 13e: the R9 technical-identifier exemption (ENGLISH_COPY_EXEMPT_SITES) must be
  // SITE-scoped exact {file, text} equality and fail-open. Pin every direction:
  // (1) an exempt site is not reported, (2) the same text in a different file still reports
  // (not word-based), (3) a different text in the same file still reports (not file-based),
  // (4) a superstring of an exempt text still reports (exact equality, not substring),
  // (5) a genuine finding inside an exempt FILE still reports (not file-wide).
  const trafficPage = "src/app/(main)/function/traffic/page.tsx";
  const mcpPage = "src/app/(main)/system/mcp/page.tsx";

  const exemptSite = analyzeSource("<div>host</div>", miniCatalog, trafficPage);
  check("english chrome: exempt {file,text} site not reported (R9, Task 13e)", exemptSite.english.length, 0);

  const exemptEllipsisSite = analyzeSource("<div>host…</div>", miniCatalog, trafficPage);
  check(
    "english chrome: 'host…' entry coexists with 'host' entry (exact match, Task 13e)",
    exemptEllipsisSite.english.length,
    0,
  );

  const sameTextOtherFile = analyzeSource("<div>host</div>", miniCatalog, mcpPage);
  check(
    "english chrome: same text in a different file still reported (not word-based)",
    sameTextOtherFile.english.map((e) => e.text),
    ["host"],
  );

  const differentTextSameFile = analyzeSource("<div>content-encoding</div>", miniCatalog, trafficPage);
  check(
    "english chrome: different text in the same file still reported (not file-based)",
    differentTextSameFile.english.map((e) => e.text),
    ["content-encoding"],
  );

  const superstringSameFile = analyzeSource("<div>host filter</div>", miniCatalog, trafficPage);
  check(
    "english chrome: superstring of an exempt text still reported (exact equality, not substring)",
    superstringSameFile.english.map((e) => e.text),
    ["host filter"],
  );

  const genuineInsideExemptFile = analyzeSource("<div>database</div>", miniCatalog, mcpPage);
  check(
    "english chrome: genuine finding inside an exempt file still reported (not file-wide)",
    genuineInsideExemptFile.english.map((e) => e.text),
    ["database"],
  );

  if (failed > 0) {
    console.error(`self-test failed: ${failed}`);
    process.exit(1);
  }
  console.log("self-test ok");
}

function report(findings, dead, opts) {
  if (findings.ambiguous.length > 0) {
    console.error(`ambiguous translator bindings (one name, multiple namespaces): ${findings.ambiguous.length}`);
    for (const item of findings.ambiguous) {
      console.error(`  ${item.relPath}:${item.line} ${item.name} -> ${item.namespaces.join(", ")}`);
    }
  }
  if (findings.ambiguousCalls.length > 0) {
    console.error(
      `ambiguous call sites (resolved against all candidate namespaces): ${findings.ambiguousCalls.length}`,
    );
    for (const item of findings.ambiguousCalls) {
      const outcome =
        item.resolved.length > 0 ? `referenced in ${item.resolved.join("|")}` : "missing in all candidates";
      const candidates = item.candidates.join("|");
      console.error(
        `  ${item.relPath}:${item.line} ${item.name}("${item.key}") [ambiguous namespace: ${candidates}] ${outcome}`,
      );
    }
  }
  if (findings.destructured.length > 0) {
    console.error(`destructuring translator bindings (classified unscoped): ${findings.destructured.length}`);
    for (const item of findings.destructured) {
      console.error(
        `  ${item.relPath}:${item.line} ${item.name} <- useTranslations(${JSON.stringify(item.namespace)})`,
      );
    }
  }
  if (findings.unscoped.length > 0) {
    console.error(`unscoped calls: ${findings.unscoped.length}`);
    for (const item of findings.unscoped) {
      console.error(`  ${item.relPath}:${item.line} ${item.name}("${item.key}") [${item.reason}]`);
    }
  }
  if (findings.dynamic.length > 0) {
    console.error(`unverifiable (dynamic) keys: ${findings.dynamic.length}`);
    for (const item of findings.dynamic) console.error(`  ${item.relPath}:${item.line} ${item.snippet}`);
  }
  if (dead.length > 0) {
    console.error(`dead keys: ${dead.length}`);
    for (const key of dead) console.error(`  ${key}`);
  }
  if (findings.english.length > 0) {
    const byFile = new Map();
    for (const item of findings.english) {
      if (!byFile.has(item.relPath)) byFile.set(item.relPath, []);
      byFile.get(item.relPath).push(item.line);
    }
    console.error(`english chrome candidates: ${findings.english.length} in ${byFile.size} files`);
    const rows = [...byFile.entries()]
      .map(([file, lines]) => ({ file, lines: [...lines].sort((a, b) => a - b) }))
      .sort((a, b) => b.lines.length - a.lines.length || a.file.localeCompare(b.file));
    for (const row of rows) console.error(`  ${row.file}: ${row.lines.length} [${row.lines.join(", ")}]`);
  }
  if (findings.missing.length > 0) {
    console.error(`missing keys: ${findings.missing.length}`);
    for (const item of findings.missing) {
      // "(any)" is the data-key placeholder for "resolved under no namespace"; it is not a real
      // namespace, so it is not printed as a scope prefix (the bare key is the honest label).
      const scope = item.namespace && item.namespace !== "(any)" ? `${item.namespace}.` : "";
      const candidates = item.candidates ? ` [ambiguous namespace: ${item.candidates.join("|")}]` : "";
      console.error(`  ${item.relPath}:${item.line} ${scope}${item.key}${candidates}`);
    }
  }

  const status = findings.missing.length === 0 ? "catalog ok" : "catalog FAILED";
  console.log(
    `${status}: missing ${findings.missing.length} | dead ${dead.length} | unscoped ${findings.unscoped.length} | ambiguous ${findings.ambiguous.length} | dynamic ${findings.dynamic.length} | english-chrome ${findings.english.length}`,
  );

  const englishStrictFailure = opts.englishStrict && findings.english.length > 0;
  if (findings.missing.length > 0 || englishStrictFailure) process.exit(1);
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  const catalog = JSON.parse(readFileSync(opts.catalog, "utf8"));

  if (opts.selfTest) {
    selfTest(catalog);
    return;
  }

  const findings = makeFindings();

  for (const file of walk(opts.source, []).sort()) {
    const relPath = toPosix(relative(projectRoot, file));
    const stripped = stripComments(readFileSync(file, "utf8"));

    if (!isExcluded(relPath, EXCLUDE_FROM_KEY_SCAN)) {
      const codeOnly = blankQuotedStrings(stripped);
      const scopes = extractScopes(stripped);
      const destructured = extractDestructuredBindings(stripped);
      const i18nAware = /(?:use|get)Translations/.test(stripped) || /from\s+["']next-intl["']/.test(stripped);
      if (scopes.size > 0 || destructured.names.size > 0 || i18nAware) {
        for (const entry of destructured.entries) findings.destructured.push({ relPath, ...entry });
        extractTranslatorFindings(
          stripped,
          codeOnly,
          relPath,
          scopes,
          destructured.names,
          i18nAware,
          catalog,
          findings,
        );
      }
      extractDataKeys(stripped, relPath, catalog, findings);
    }
    if (!isExcluded(relPath, EXCLUDE_FROM_ENGLISH_SCAN)) {
      extractEnglishChrome(stripped, relPath, findings, relPath.endsWith(".tsx"), catalog);
    }
  }

  if (!existsSync(SIDEBAR_ITEMS_PATH)) {
    console.error(
      `note: canonical navigation data module not found at ${toPosix(relative(projectRoot, SIDEBAR_ITEMS_PATH))}; titleKey/labelKey literals are still scanned generically across src.`,
    );
  }

  const leaves = [];
  collectLeafPaths(catalog, "", leaves);
  const dead = leaves.filter((path) => !findings.referenced.has(path));

  report(findings, dead, opts);
}

main();
