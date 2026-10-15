// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: intent / coverage / task / severity each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose is used for "critical": a solid, high-emphasis fill that is deliberately
  // more prominent than the soft red outline "high" uses ("critical" vs "high").
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

export interface StatusMeta {
  tone: Tone;
  // Fully qualified catalog path (e.g. "status.severity.critical"), or null when
  // the key is not part of the domain map and no label can be resolved.
  labelKey: string | null;
}

// Each domain maps runtime keys to a tone plus a fully qualified catalog key. The
// catalog segment is the map key, never the old display string.
interface StatusEntry {
  tone: Tone;
  labelKey: string;
}

const intent: Record<string, StatusEntry> = {
  open: { tone: "slate", labelKey: "status.intent.open" },
  running: { tone: "blue", labelKey: "status.intent.running" },
  paused: { tone: "amber", labelKey: "status.intent.paused" },
  done: { tone: "green", labelKey: "status.intent.done" },
  // blocked = model/API/network failures exhausted the retries, so the intent was
  // never actually explored (this is not a target block).
  blocked: { tone: "red", labelKey: "status.intent.blocked" },
  // exhausted = cut off mid-run by the step/time budget with only partial results
  // written back (this is not "all directions explored").
  exhausted: { tone: "violet", labelKey: "status.intent.exhausted" },
  // stopped = legacy soft-delete state, kept for historical data.
  stopped: { tone: "slate", labelKey: "status.intent.stopped" },
  // deleted = the user soft-deleted this intent; nodes and lineage are retained and
  // the reason lives in the delete_reason field.
  deleted: { tone: "slate", labelKey: "status.intent.deleted" },
};

const task: Record<string, StatusEntry> = {
  created: { tone: "slate", labelKey: "status.task.created" },
  queued: { tone: "amber", labelKey: "status.task.queued" },
  running: { tone: "blue", labelKey: "status.task.running" },
  paused: { tone: "amber", labelKey: "status.task.paused" },
  done: { tone: "green", labelKey: "status.task.done" },
  failed: { tone: "red", labelKey: "status.task.failed" },
  timeout: { tone: "amber", labelKey: "status.task.timeout" },
};

const severity: Record<string, StatusEntry> = {
  critical: { tone: "rose", labelKey: "status.severity.critical" },
  high: { tone: "red", labelKey: "status.severity.high" },
  medium: { tone: "amber", labelKey: "status.severity.medium" },
  low: { tone: "slate", labelKey: "status.severity.low" },
};

const finding: Record<string, StatusEntry> = {
  pending: { tone: "amber", labelKey: "status.finding.pending" },
  in_progress: { tone: "blue", labelKey: "status.finding.in_progress" },
  confirmed: { tone: "red", labelKey: "status.finding.confirmed" },
  resolved: { tone: "green", labelKey: "status.finding.resolved" },
  fixed: { tone: "green", labelKey: "status.finding.fixed" },
  false_positive: { tone: "slate", labelKey: "status.finding.false_positive" },
  ignored: { tone: "neutral", labelKey: "status.finding.ignored" },
  duplicate: { tone: "neutral", labelKey: "status.finding.duplicate" },
  risk_accepted: { tone: "violet", labelKey: "status.finding.risk_accepted" },
};

const engine: Record<string, StatusEntry> = {
  exploring: { tone: "blue", labelKey: "status.engine.exploring" },
  paused: { tone: "amber", labelKey: "status.engine.paused" },
  stalled: { tone: "red", labelKey: "status.engine.stalled" },
  idle: { tone: "neutral", labelKey: "status.engine.idle" },
};

const goal: Record<string, StatusEntry> = {
  open: { tone: "blue", labelKey: "status.goal.open" },
  met: { tone: "green", labelKey: "status.goal.met" },
  abandoned: { tone: "slate", labelKey: "status.goal.abandoned" },
};

const audit: Record<string, StatusEntry> = {
  allow: { tone: "green", labelKey: "status.audit.allow" },
  block: { tone: "red", labelKey: "status.audit.block" },
};

const node: Record<string, StatusEntry> = {
  observed: { tone: "slate", labelKey: "status.node.observed" },
  confirmed: { tone: "green", labelKey: "status.node.confirmed" },
  tombstoned: { tone: "neutral", labelKey: "status.node.tombstoned" },
};

// Notification delivery status. sending uses blue rather than amber: it is not
// "problematic" but "claimed and currently being sent", which must stay
// distinguishable from pending's waiting semantics.
const delivery: Record<string, StatusEntry> = {
  pending: { tone: "amber", labelKey: "status.delivery.pending" },
  sending: { tone: "blue", labelKey: "status.delivery.sending" },
  sent: { tone: "green", labelKey: "status.delivery.sent" },
  failed: { tone: "red", labelKey: "status.delivery.failed" },
  skipped: { tone: "neutral", labelKey: "status.delivery.skipped" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  const entry: StatusEntry | undefined = maps[domain][key];
  return entry ?? { tone: "neutral", labelKey: null };
}

// Test seam: yields every { domain, key, labelKey } triple the label maps know, so the
// catalog-resolution test can enumerate the maps themselves instead of mirroring them.
export function listStatusLabelEntries(): { domain: StatusDomain; key: string; labelKey: string }[] {
  // `Object.keys` widens to string[]; the only cast narrows it back to the maps' own key union.
  const domains = Object.keys(maps) as StatusDomain[];
  return domains.flatMap((domain) =>
    Object.entries(maps[domain]).map(([key, entry]) => ({ domain, key, labelKey: entry.labelKey })),
  );
}

const STATUS_NAMESPACE_PREFIX = "status.";

// Resolves a domain key to its localised label. Unknown keys fall back to the raw
// key so the UI never renders an empty badge.
//
// labelKey stores a fully qualified path ("status.<domain>.<key>") while `t` is
// expected to be bound to the "status" namespace. next-intl resolves keys
// *relative* to the bound namespace, so the leading namespace segment must be
// stripped before translating; passing the qualified path through verbatim would
// look up "status.status.<domain>.<key>" and miss.
export function statusLabel(domain: StatusDomain, key: string, t: (key: string) => string): string {
  const { labelKey } = statusMeta(domain, key);
  return labelKey === null ? key : t(labelKey.slice(STATUS_NAMESPACE_PREFIX.length));
}
