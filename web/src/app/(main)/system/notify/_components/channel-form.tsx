"use client";

import { CheckIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType are value helpers local to this file (tightly coupled to
// control rendering), so they do not live in channel-fields.
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText renders any config value as a string the inputs can hold.
// config comes from JSON, so the value may be string / number / boolean /
// array / null; we only care about "can it go into a text field" — actual
// serialization is buildConfig's job.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType maps a field kind to the input's type attribute.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField renders the control that matches the field definition.
//
// Masked fields are the only subtlety here: the input **does not display** the
// masked value itself, only a one-line "already saved" hint. That keeps the UI
// to a single rule — text in the box means the user typed it, empty means
// empty. If we put "__masked__:…abc123" into the input, users would read it as
// placeholder text meant to be deleted, which makes accidental credential
// wipes more likely.
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const t = useTranslations("notify");
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // Masked value echoed by the backend: "__masked__:…abc123", where the tail
  // is an identifiable fragment of the original value.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={t(def.label)} />
        {t(def.label)}
        {def.help && <span className="text-muted-foreground">（{t.raw(def.help)}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{t(def.label)}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {t(o.label)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // Controls are dispatched by field kind. An if chain rather than nested
  // ternaries because there are four controls to distinguish — a three-level
  // ternary already forces readers to stop and count parentheses.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  // t.raw: some help values contain literal `{{...}}` template braces, which
  // plain t() would feed to ICU and throw on.
  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      {t("maskSaved")}
      {maskedTail && t("maskSavedTail", { tail: maskedTail })} · {t("maskHint")}
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{t.raw(def.help)}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{t(def.label)}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary condenses the filter conditions into one line, so a channel
// card shows what it will push without being opened.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const t = useTranslations("notify");
  const parts: string[] = [];
  if (filter.min_severity) {
    const sev = SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity);
    parts.push(sev ? t(sev.label) : filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(t("summaryTypes", { count: filter.vulnclass_include.length }));
  if (filter.vulnclass_exclude?.length) parts.push(t("summaryExcludes", { count: filter.vulnclass_exclude.length }));
  if (filter.task_ids?.length) parts.push(t("summaryTasks", { count: filter.task_ids.length }));
  if (filter.asset_ids?.length) parts.push(t("summaryAssets", { count: filter.asset_ids.length }));
  if (filter.on_status_change) parts.push(t("summaryStatusChange"));
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">{t("summaryAll")}</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
