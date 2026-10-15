import { Card, CardContent } from "@/components/ui/card";

export function StatTile({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: string }) {
  return (
    <Card size="sm" className="gap-1">
      <CardContent>
        <p className="text-muted-foreground text-xs">{label}</p>
        <p className={`text-lg font-semibold ${tone === "red" ? "text-rose-600" : ""}`}>{value}</p>
        {hint && <p className={`text-xs ${tone === "red" ? "text-rose-600" : "text-muted-foreground"}`}>{hint}</p>}
      </CardContent>
    </Card>
  );
}

// formatBacklog renders the backlog age in milliseconds at a human-readable
// magnitude. Unit strings live in the `notify` catalog, so callers pass their
// `useTranslations("notify")` translator in (same t-as-argument pattern as
// statusLabel in lib/status.ts).
export function formatBacklog(
  ms: number,
  t: (key: string, values?: Record<string, string | number>) => string,
): string {
  if (!ms) return "—";
  if (ms < 60_000) return t("unitSeconds", { value: Math.round(ms / 1000) });
  if (ms < 3_600_000) return t("unitMinutes", { value: Math.round(ms / 60_000) });
  return t("unitHours", { value: (ms / 3_600_000).toFixed(1) });
}
