"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** Max time to wait for the new version to come up. A single upgrade goes through three
 *  process starts (staging → swap → new version), each taking seconds; three minutes
 *  comfortably covers slow disks and Docker container rebuilds. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // Kept separate from progress: once staging completes the process is gone, the SSE stream
  // drops, and we switch to polling /api/health.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const t = useTranslations("settings");

  // quiet also decides whether to bypass the backend cache: the automatic check on page entry
  // uses the cache (the top bar just checked), while a manual "Check for updates" forces a
  // refresh — otherwise a freshly published version stays hidden until the cache expires.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error(t("update.checkFailed", { message: r.error }));
          else if (r.has_update) toast.success(t("update.newVersion", { version: r.latest ?? "" }));
          else if (r.comparable) toast.success(t("update.upToDate"));
        }
      })
      .catch((e) => {
        if (!quiet) toast.error(t("update.checkFailed", { message: (e as Error).message }));
      })
      .finally(() => setChecking(false));
  }, [t]);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // Poll /api/health until the version string changes.
  //
  // The criterion must be "version changed", not "reachable": during the swap the old
  // version briefly comes back up once (that run only swaps in artex.new and exits
  // immediately), so checking connectivity alone would falsely report success.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(t("update.updatedReload", { version: j.version }));
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // Being unreachable during the restart window is expected; keep polling.
      }
    }
    setRestarting(false);
    toast.error(t("update.restartTimeout"));
  }, [t]);

  // Subscribe to update progress. SSE does not go through Next's /api rewrite (that layer
  // buffers and no events get through).
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error(t("update.updateFailed", { message: p.error || p.message }));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // The SSE stream necessarily drops when the process exits. If we have already entered
        // the waiting-for-restart phase this is normal — let the /api/health polling keep
        // deciding.
        es.close();
      };
      return es;
    },
    [waitForNewVersion, t],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${t("update.confirmUpdateTitle", { version: info.latest ?? "" })}\n\n` +
        `${t("update.confirmUpdateBody")}\n` +
        (info.mode === "docker" ? `\n${t("update.confirmUpdateDocker")}` : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: t("update.prepare") });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error(t("update.startUpdateFailed", { message: (e as Error).message }));
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (!window.confirm(`${t("update.confirmRollbackTitle")}\n\n${t("update.confirmRollbackBody")}`)) return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success(t("update.switchedPrevious"));
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error(t("update.rollbackFailed", { message: (e as Error).message }));
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // Only the download phase has a real percentage (computed from Content-Length).
  // Verify/extract/wait-for-restart have unknown durations, so the bar fills up and pulses to
  // signal "busy, but uncertain how much longer".
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // The settings page uses a multi-column masonry layout; each card owns its row spacing and
    // forbids breaking across columns (see the note in page.tsx).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          {t("update.title")}
        </CardTitle>
        <CardDescription>{t("update.description")}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">{t("update.current")}</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : t("update.standalone")}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">{t("update.latest")}</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              {t("update.changelog")} <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {t("update.githubUnreachable", { message: info.error })}
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {t("update.assetMissing", {
              version: info.latest ?? "",
              os: info.os,
              arch: info.arch,
              asset: info.asset ?? "",
            })}
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            {t.rich("update.willDownload", {
              code: (chunks) => <span className="font-mono">{chunks}</span>,
              asset: info.asset ?? "",
              size: info.size ? `（${humanSize(info.size)}）` : "",
            })}
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            {t("update.upToDate")}
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            {t.rich("update.dockerNote", { code: (chunks) => <span className="font-mono">{chunks}</span> })}
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? t("update.restarting") : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            {t("update.check")}
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? t("update.updateTo", { version: info.latest ?? "" }) : t("update.updateNow")}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              {t("update.rollback")}
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          {t.rich("update.daemonNote", { code: (chunks) => <span className="font-mono">{chunks}</span> })}
        </p>
      </CardContent>
    </Card>
  );
}
