"use client";

import * as React from "react";

import { useTranslations } from "next-intl";

import { Badge } from "@/components/ui/badge";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Textarea } from "@/components/ui/textarea";
import { MAX_COMPANY_SCOPE_VALUE_LENGTH, type ParsedCompanyScopeText } from "@/lib/company-scope";
import type { CompanyScopeKind } from "@/lib/types";

export function ScopeTextEditor({
  id,
  value,
  onValueChange,
  parsed,
  label,
  description,
}: {
  id: string;
  value: string;
  onValueChange: (value: string) => void;
  parsed: ParsedCompanyScopeText;
  label?: string;
  description?: string;
}) {
  const t = useTranslations("components");
  const tc = useTranslations("common");
  const SCOPE_KIND_LABELS: Record<CompanyScopeKind, string> = {
    domain: t("stDomain"),
    ip: "IP",
    cidr: "CIDR",
    icp: "ICP",
    keyword: t("stKeyword"),
  };
  // company-scope validators return catalog error codes, not display copy (the lib cannot
  // call hooks). An if/else chain with literal t() calls keeps every key statically
  // resolvable for the catalog checker; unknown codes pass through unchanged.
  const scopeErrorText = (code: string): string => {
    if (code === "scopeEmpty") return tc("scopeEmpty");
    if (code === "scopeMaxLen") return tc("scopeMaxLen", { max: MAX_COMPANY_SCOPE_VALUE_LENGTH });
    if (code === "scopeDomainNoSpaces") return tc("scopeDomainNoSpaces");
    if (code === "scopeInvalidDomainOrUrl") return tc("scopeInvalidDomainOrUrl");
    if (code === "scopeInvalidIp") return tc("scopeInvalidIp");
    if (code === "scopeInvalidCidr") return tc("scopeInvalidCidr");
    if (code === "scopeIPv4Prefix") return tc("scopeIPv4Prefix");
    if (code === "scopeIPv6Prefix") return tc("scopeIPv6Prefix");
    return code;
  };
  const counts = React.useMemo(() => {
    const result = new Map<CompanyScopeKind, number>();
    for (const rule of parsed.rules) result.set(rule.kind, (result.get(rule.kind) ?? 0) + 1);
    return result;
  }, [parsed.rules]);

  return (
    <Field data-invalid={parsed.errors.length > 0}>
      <FieldLabel htmlFor={id}>{label ?? t("stLabel")}</FieldLabel>
      <FieldDescription>{description ?? t("stDescription")}</FieldDescription>
      <Textarea
        id={id}
        rows={8}
        value={value}
        aria-invalid={parsed.errors.length > 0}
        placeholder={t("stPlaceholder")}
        className="min-h-36 resize-y font-mono text-sm"
        onChange={(event) => onValueChange(event.target.value)}
      />
      {parsed.rules.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5 text-muted-foreground text-xs">
          <span>{t("stRecognized", { n: parsed.rules.length })}</span>
          {Object.entries(SCOPE_KIND_LABELS).map(([kind, kindLabel]) => {
            const count = counts.get(kind as CompanyScopeKind) ?? 0;
            return count > 0 ? (
              <Badge key={kind} variant="secondary" className="font-mono tabular-nums">
                {kindLabel} {count}
              </Badge>
            ) : null;
          })}
        </div>
      )}
      {parsed.errors.length > 0 && (
        <FieldError>
          {parsed.errors.slice(0, 5).map((item) => (
            <span key={`${item.line}-${item.error}`} className="block">
              {t("stErrorLine", { line: item.line, error: scopeErrorText(item.error) })}
            </span>
          ))}
          {parsed.errors.length > 5 && (
            <span className="block">{t("stMoreErrors", { n: parsed.errors.length - 5 })}</span>
          )}
        </FieldError>
      )}
    </Field>
  );
}
