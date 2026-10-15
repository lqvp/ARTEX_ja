"use client";

import * as React from "react";

import { SearchIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// ── DSL autocomplete ──────────────────────────────────────────────────────────
// Shared by the global asset view (/function/assets) and the per-task test-asset
// search, so both search boxes behave and look identical.

// Pure syntax table: the human-readable wording for fields and operators lives in
// the message catalog (see dslFieldDesc / dslOpDesc below).
const DSL_FIELDS: { name: string; ops: string[] }[] = [
  { name: "domain", ops: ["=", "==", "!="] },
  { name: "ip", ops: ["=", "==", "!="] },
  { name: "url", ops: ["=", "==", "!="] },
  { name: "root_domain", ops: ["=", "==", "!="] },
  { name: "page_title", ops: ["=", "==", "!="] },
  { name: "icp", ops: ["=", "==", "!="] },
  { name: "service_name", ops: ["=", "==", "!="] },
  { name: "app_name", ops: ["=", "==", "!="] },
  { name: "bundle_id", ops: ["=", "==", "!="] },
  { name: "category", ops: ["=", "==", "!="] },
  { name: "app_icp", ops: ["=", "==", "!="] },
  { name: "method", ops: ["==", "!="] },
  { name: "service_type", ops: ["==", "!="] },
  { name: "record_type", ops: ["==", "!="] },
  { name: "technology", ops: ["=", "==", "!="] },
  { name: "port", ops: ["==", "!=", ">", ">=", "<", "<="] },
  { name: "status_code", ops: ["==", "!=", ">", ">=", "<", "<="] },
  { name: "company_id", ops: ["=="] },
  { name: "task_id", ops: ["=="] },
];

const LOGIC_OPS = [{ label: "AND" }, { label: "OR" }];

type Translate = ReturnType<typeof useTranslations>;

// Field descriptions for the autocomplete. An if/else chain with literal t() calls
// keeps every catalog key statically resolvable (the checker cannot see Record
// values whose property names do not match the key name); an unknown field falls
// through to its raw name, exactly as the table above never lists one.
function dslFieldDesc(name: string, t: Translate): string {
  if (name === "domain") return t("fieldDomain");
  if (name === "ip") return t("fieldIp");
  if (name === "url") return t("fieldUrl");
  if (name === "root_domain") return t("fieldRootDomain");
  if (name === "page_title") return t("fieldPageTitle");
  if (name === "icp") return t("fieldIcp");
  if (name === "service_name") return t("fieldServiceName");
  if (name === "app_name") return t("fieldAppName");
  if (name === "bundle_id") return t("fieldBundleId");
  if (name === "category") return t("fieldCategory");
  if (name === "app_icp") return t("fieldAppIcp");
  if (name === "method") return t("fieldMethod");
  if (name === "service_type") return t("fieldServiceType");
  if (name === "record_type") return t("fieldRecordType");
  if (name === "technology") return t("fieldTechnology");
  if (name === "port") return t("fieldPort");
  if (name === "status_code") return t("fieldStatusCode");
  if (name === "company_id") return t("fieldCompanyId");
  if (name === "task_id") return t("fieldTaskId");
  return name;
}

// Operator descriptions. The numeric fields word the comparison operators
// differently from the string fields, so the field name selects the wording.
function dslOpDesc(field: string, op: string, t: Translate): string {
  if (field === "port" || field === "status_code" || field === "company_id" || field === "task_id") {
    if (op === "==") return t("opEquals");
    if (op === "!=") return t("opNotEquals");
    if (op === ">") return t("opGreater");
    if (op === ">=") return t("opGreaterEqual");
    if (op === "<") return t("opLess");
    return t("opLessEqual");
  }
  if (op === "=") return t("opFuzzy");
  if (op === "==") return t("opExact");
  return t("opExclude");
}

function dslLogicDesc(label: string, t: Translate): string {
  if (label === "AND") return t("logicAnd");
  if (label === "OR") return t("logicOr");
  return label;
}

interface DslSuggestion {
  kind: "field" | "operator" | "logic";
  label: string;
  desc: string;
  replaceStart: number;
  replaceEnd: number;
  insertText: string;
}

function getDslSuggestions(text: string, cursor: number, t: Translate): DslSuggestion[] {
  const before = text.slice(0, cursor);
  // Current token: non-whitespace, non-paren run ending at cursor
  const tokenMatch = before.match(/([^\s()]*$)/);
  const currentToken = tokenMatch?.[1] ?? "";
  const tokenStart = cursor - currentToken.length;

  // Token already contains field+operator → typing a value, no suggestions
  if (/^[a-z_]+(==|!=|>=|<=|=|>|<)/.test(currentToken)) return [];

  // Complete known field name → suggest operators for that field
  const exactField = DSL_FIELDS.find((f) => f.name === currentToken.toLowerCase());
  if (exactField) {
    return exactField.ops.map((op) => ({
      kind: "operator",
      label: `${exactField.name}${op}`,
      desc: dslOpDesc(exactField.name, op, t),
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${exactField.name}${op}`,
    }));
  }

  // Everything before the current token (trimmed)
  const beforeToken = before.slice(0, tokenStart).trimEnd();
  const afterExpression = beforeToken.length > 0 && !/\b(AND|OR)\s*$/i.test(beforeToken) && !beforeToken.endsWith("(");

  // Current token is a prefix of AND/OR and follows a complete expression
  if (/^(a|an|and|o|or)$/i.test(currentToken) && afterExpression) {
    return LOGIC_OPS.filter((l) => l.label.startsWith(currentToken.toUpperCase())).map(({ label }) => ({
      kind: "logic",
      label,
      desc: dslLogicDesc(label, t),
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // No current token, after a complete expression → suggest AND/OR
  if (!currentToken && afterExpression) {
    return LOGIC_OPS.map(({ label }) => ({
      kind: "logic",
      label,
      desc: dslLogicDesc(label, t),
      replaceStart: cursor,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // Default: suggest fields filtered by prefix
  const prefix = currentToken.toLowerCase();
  return DSL_FIELDS.filter((f) => f.name.startsWith(prefix)).map((f) => ({
    kind: "field",
    label: f.name,
    desc: dslFieldDesc(f.name, t),
    replaceStart: tokenStart,
    replaceEnd: cursor,
    insertText: f.name,
  }));
}

function applyDslSuggestion(text: string, s: DslSuggestion): { text: string; cursor: number } {
  const newText = text.slice(0, s.replaceStart) + s.insertText + text.slice(s.replaceEnd);
  return { text: newText, cursor: s.replaceStart + s.insertText.length };
}

const KIND_STYLE: Record<string, string> = {
  field: "text-blue-500 dark:text-blue-400",
  operator: "text-amber-500 dark:text-amber-400",
  logic: "text-emerald-500 dark:text-emerald-400",
};

// AssetDslSearch is the shared DSL search box: a monospace input with a
// field/operator/logic autocomplete popover and a status line ("N found" /
// error / loading). Used by both the global asset view and the per-task view.
export function AssetDslSearch({
  query,
  onChange,
  loading,
  error,
  count,
}: {
  query: string;
  onChange: (v: string) => void;
  loading: boolean;
  error: string;
  count?: number;
}) {
  const t = useTranslations("components");
  const inputRef = React.useRef<HTMLInputElement>(null);
  const [suggestions, setSuggestions] = React.useState<DslSuggestion[]>([]);
  const [selIdx, setSelIdx] = React.useState(0);
  const [open, setOpen] = React.useState(false);
  const foundText = t("foundN", { n: count ?? 0 });

  const refresh = React.useCallback(
    (val: string, pos: number) => {
      const suggs = getDslSuggestions(val, pos, t);
      setSuggestions(suggs);
      setSelIdx(0);
      setOpen(suggs.length > 0);
    },
    [t],
  );

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const val = e.target.value;
    onChange(val);
    refresh(val, e.target.selectionStart ?? val.length);
  };

  const apply = React.useCallback(
    (s: DslSuggestion) => {
      const cursor = inputRef.current?.selectionStart ?? query.length;
      // use cursor for logic-kind (insert at cursor), replaceStart/End for others
      const adjusted: DslSuggestion =
        s.kind === "logic" && !query.slice(s.replaceStart, s.replaceEnd)
          ? { ...s, replaceStart: cursor, replaceEnd: cursor }
          : s;
      const { text: newText, cursor: newCursor } = applyDslSuggestion(query, adjusted);
      onChange(newText);
      requestAnimationFrame(() => {
        if (!inputRef.current) return;
        inputRef.current.setSelectionRange(newCursor, newCursor);
        inputRef.current.focus();
        refresh(newText, newCursor);
      });
    },
    [query, onChange, refresh],
  );

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open || suggestions.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelIdx((i) => Math.min(i + 1, suggestions.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelIdx((i) => Math.max(i - 1, 0));
    } else if (e.key === "Tab" || e.key === "Enter") {
      const s = suggestions[selIdx];
      if (s) {
        e.preventDefault();
        apply(s);
      }
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  const cursorPos = () => inputRef.current?.selectionStart ?? query.length;

  return (
    <div className="flex flex-col gap-1">
      <div className="relative max-w-lg">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          ref={inputRef}
          placeholder={t("placeholder")}
          value={query}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onFocus={() => refresh(query, cursorPos())}
          onClick={() => refresh(query, cursorPos())}
          onBlur={() => setTimeout(() => setOpen(false), 120)}
          className="h-8 pl-8 font-mono text-xs"
        />
        {open && suggestions.length > 0 && (
          <div className="absolute top-full left-0 z-50 mt-1 w-max min-w-full max-w-sm rounded-md border bg-popover py-1 shadow-md">
            {suggestions.map((s, i) => (
              <button
                type="button"
                key={i}
                className={cn(
                  "flex w-full cursor-pointer items-center gap-3 px-3 py-1.5 text-left",
                  i === selIdx ? "bg-accent" : "hover:bg-accent/50",
                )}
                onMouseEnter={() => setSelIdx(i)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  apply(s);
                }}
              >
                <span className={cn("shrink-0 font-mono text-xs font-semibold", KIND_STYLE[s.kind])}>{s.label}</span>
                <span className="text-xs text-muted-foreground">{s.desc}</span>
              </button>
            ))}
          </div>
        )}
      </div>
      {query.trim() && !open && (
        <p className="pl-1 text-[11px] text-muted-foreground">
          {loading ? t("searching") : error ? <span className="text-destructive">{error}</span> : foundText}
        </p>
      )}
    </div>
  );
}
