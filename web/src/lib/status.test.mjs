import { listStatusLabelEntries, statusLabel, statusMeta } from "./status.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";

const catalog = JSON.parse(readFileSync(join(import.meta.dirname, "../messages/ja.json"), "utf8"));

test("unknown key falls back to neutral tone and a null labelKey", () => {
  assert.deepEqual(statusMeta("task", "totally-unknown"), {
    tone: "neutral",
    labelKey: null,
  });
});

test("known key carries a dotted catalog labelKey", () => {
  const meta = statusMeta("severity", "critical");
  assert.equal(meta.tone, "rose");
  assert.equal(meta.labelKey, "status.severity.critical");
});

test("every domain and key resolves to a labelKey", () => {
  for (const domain of ["intent", "task", "severity", "finding", "engine", "goal", "audit", "node", "delivery"]) {
    for (const key of ["critical", "running", "done", "failed"]) {
      const meta = statusMeta(domain, key);
      if (meta.labelKey !== null) assert.match(meta.labelKey, /^status\./);
    }
  }
});

test("statusLabel strips the namespace segment before translating", () => {
  const seen = [];
  const t = (key) => {
    seen.push(key);
    return `translated:${key}`;
  };
  assert.equal(statusLabel("severity", "critical", t), "translated:severity.critical");
  assert.deepEqual(seen, ["severity.critical"]);
});

test("statusLabel falls back to the raw key when the domain has no label", () => {
  const t = (key) => `translated:${key}`;
  assert.equal(statusLabel("task", "totally-unknown", t), "totally-unknown");
});

// Resolves a dotted catalog path. Returns undefined when any segment is missing or
// lands on a non-object, so an intermediate namespace object can never pass as a leaf.
function resolveCatalogPath(path) {
  let node = catalog;
  for (const part of path.split(".")) {
    if (node === null || typeof node !== "object") return undefined;
    node = node[part];
  }
  return node;
}

// The static catalog audit cannot see statusLabel's dynamic `t(labelKey.slice(...))`
// call, so this is the safety net: every map labelKey must point at a real, non-empty
// catalog string. It enumerates the maps via listStatusLabelEntries() rather than a
// copy, so a key added to status.ts is covered automatically. A misspelled key, a
// renamed/missing namespace key, or a map entry with no catalog leaf fails here.
test("every domain labelKey resolves to a non-empty catalog string", () => {
  const entries = listStatusLabelEntries();
  assert.ok(entries.length > 0, "the enumerator should yield the domain map entries");
  for (const { domain, key, labelKey } of entries) {
    assert.equal(labelKey, `status.${domain}.${key}`, `${domain}.${key} should carry its domain-scoped labelKey`);
    const value = resolveCatalogPath(labelKey);
    assert.equal(typeof value, "string", `${labelKey} must resolve to a catalog string leaf`);
    assert.ok(value.length > 0, `${labelKey} must be a non-empty string`);
  }
});
