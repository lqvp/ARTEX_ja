"use client";

// Shared widgets for the LLM retry configuration: the "attempts + interval"
// control for each of the five retry layers.
//
// The five layers, innermost to outermost: connect (SDK) → empty response (SDK)
// → same-provider safety window → pool circuit breaker → intent rerun.
// The inner three follow the endpoint, so every model profile can override the
// global default; the outer two are process-level and exist once, globally.
//
// Every input follows the same "blank = unset" semantics, matching backend
// db.RetryRule:
//   attempts   blank/0 = use built-in default | -1 = disable this layer | >0 = use this count
//   interval   blank/0 = keep this layer's original exponential backoff | >0 = use this fixed ms interval

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

// Static per-layer metadata: display copy lives in the llm.retryLayer catalog,
// referenced through LAYER_MESSAGES below.
const RETRY_LAYERS = {
  connect: { defAttempts: 3 },
  empty: { defAttempts: 2 },
  stream: { defAttempts: 2 },
  breaker: { defAttempts: 3 },
  intent: { defAttempts: 2 },
} satisfies Record<string, { defAttempts: number }>;

// Catalog keys for each retry layer, referenced dynamically via t(keys.x).
// The audit reports these 35 keys as dead (it cannot follow dynamic t() calls);
// that is a known, declared limitation — see ledger Ruling 40.
const LAYER_MESSAGES = {
  connect: {
    title: "retryLayer.connect.title",
    where: "retryLayer.connect.where",
    trigger: "retryLayer.connect.trigger",
    skips: "retryLayer.connect.skips",
    desc: "retryLayer.connect.desc",
    defInterval: "retryLayer.connect.defInterval",
    offHint: "retryLayer.connect.offHint",
  },
  empty: {
    title: "retryLayer.empty.title",
    where: "retryLayer.empty.where",
    trigger: "retryLayer.empty.trigger",
    skips: "retryLayer.empty.skips",
    desc: "retryLayer.empty.desc",
    defInterval: "retryLayer.empty.defInterval",
    offHint: "retryLayer.empty.offHint",
  },
  stream: {
    title: "retryLayer.stream.title",
    where: "retryLayer.stream.where",
    trigger: "retryLayer.stream.trigger",
    skips: "retryLayer.stream.skips",
    desc: "retryLayer.stream.desc",
    defInterval: "retryLayer.stream.defInterval",
    offHint: "retryLayer.stream.offHint",
  },
  breaker: {
    title: "retryLayer.breaker.title",
    where: "retryLayer.breaker.where",
    trigger: "retryLayer.breaker.trigger",
    skips: "retryLayer.breaker.skips",
    desc: "retryLayer.breaker.desc",
    defInterval: "retryLayer.breaker.defInterval",
    offHint: "retryLayer.breaker.offHint",
  },
  intent: {
    title: "retryLayer.intent.title",
    where: "retryLayer.intent.where",
    trigger: "retryLayer.intent.trigger",
    skips: "retryLayer.intent.skips",
    desc: "retryLayer.intent.desc",
    defInterval: "retryLayer.intent.defInterval",
    offHint: "retryLayer.intent.offHint",
  },
} satisfies Record<LayerKey, Record<LayerField, string>>;

type LayerKey = keyof typeof RETRY_LAYERS;
type LayerField = "title" | "where" | "trigger" | "skips" | "desc" | "defInterval" | "offHint";

/** Human-readable milliseconds — only echoed next to the input so nobody has to count zeros. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** Controlled numeric input: empty string ↔ 0; intermediate states ("-", "1e") stay local and do not bother the parent. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // Follow along when the parent swaps in a whole new value set (policy reload,
  // profile switch); typing yourself never reaches this path, because by then
  // value already equals the locally parsed text.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** The two knobs of one retry layer. idPrefix keeps htmlFor valid when the same page renders the layer twice. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = the compact variant inside the profile sheet: drop the expanded explanation, keep only "which errors reach this layer" */
  compact?: boolean;
}) {
  const t = useTranslations("llm");
  const meta = RETRY_LAYERS[layer];
  const keys = LAYER_MESSAGES[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{t(keys.title)}</Label>
          <span className="text-muted-foreground text-xs">{t(keys.where)}</span>
        </div>
        {/* Exactly which errors reach this layer, down to the status code — turning a knob without
            seeing any effect usually means the error never lands in this layer. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">{t("retryTrigger")}</span>：{t(keys.trigger)}
        </p>
        {!compact && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">{t("retrySkips")}</span>：{t(keys.skips)}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{t(keys.desc)}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {t("retryAttempts")}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={t("retryAttemptsPlaceholder", { attempts: meta.defAttempts })}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            {t("retryIntervalLabel")}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder={t("retryIntervalPlaceholder")}
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">
            {human ? t("retryFixedInterval", { interval: human }) : t(keys.defInterval)}
          </span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">{t("retryHintLine", { offHint: t(keys.offHint) })}</p>}
    </div>
  );
}

/** The three endpoint-level retry overrides inside the model-profile sheet. */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  const t = useTranslations("llm");
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">{t("retryTitle")}</Label>
        <p className="text-muted-foreground text-xs">{t("retryDesc")}</p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** The "Retry & backoff" tab: global defaults for all five layers. */
export function RetryPolicyPanel() {
  const t = useTranslations("llm");
  const tc = useTranslations("common");
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(t("retryLoadFailed", { message: (e as Error).message }));
    } finally {
      setLoading(false);
    }
  }, [t]);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // The backend clamps out-of-range values back into bounds and echoes them;
      // refresh straight from the echoed value, so what you see is what was stored.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success(t("retrySaved"));
    } catch (e) {
      toast.error(t("retrySaveFailed", { message: (e as Error).message }));
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> {t("retryLoading")}
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        {t.rich("retryPanelIntro", { em: (chunks) => <span className="text-foreground">{chunks}</span> })}
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          {tc("save")}
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          {t("retryResetDefaults")}
        </Button>
      </div>
    </div>
  );
}
