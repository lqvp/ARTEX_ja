"use client";

import * as React from "react";

import { useTranslations } from "next-intl";

import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { Activity, InterceptExecution } from "@/lib/types";

// Resolve one exact persisted call, independently of transcript pagination.
export function useApprovalFocus({ taskId, conversationId }: { taskId?: string; conversationId?: number }) {
  const t = useTranslations("components");
  const [state, setState] = React.useState<{
    id: number;
    source?: InterceptExecution;
    error?: string;
    loading: boolean;
  } | null>(null);
  const [revision, setRevision] = React.useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: revision is the explicit retry trigger.
  React.useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const id = Number(q.get("approval"));
    const matches = conversationId ? q.get("c") === String(conversationId) : q.get("id") === taskId;
    if (!matches || !Number.isSafeInteger(id) || id <= 0) {
      setState(null);
      return;
    }
    let cancelled = false;
    setState({ id, loading: true });
    api
      .interceptExecution(id, conversationId)
      .then((source) => {
        if (cancelled) return;
        if (
          conversationId
            ? source.conversation_id !== conversationId
            : source.task_id !== taskId || source.conversation_id != null
        ) {
          throw new Error(t("afSourceMismatch"));
        }
        setState({ id, source, loading: false });
      })
      .catch((e) => {
        const message = (e as Error).message;
        if (!cancelled) setState({ id, error: message ? message : t("afLocateFallback"), loading: false });
      });
    return () => {
      cancelled = true;
    };
  }, [taskId, conversationId, revision, t]);
  const close = React.useCallback(() => {
    const url = new URL(window.location.href);
    url.searchParams.delete("approval");
    window.history.replaceState(null, "", url);
    setState(null);
  }, []);
  return { state, close, retry: () => setRevision((v) => v + 1) };
}

type HistoryPage = { items: Activity[]; hasMore: boolean };

// Fill the existing transcript continuously back to the call, preserving all
// intervening messages. Never splice an isolated call into a paginated history.
export function useApprovalHistory(
  source: InterceptExecution | undefined,
  loaded: boolean,
  items: Activity[],
  loadPage: (before: number) => Promise<HistoryPage>,
  mergePage: (page: HistoryPage) => void,
) {
  const itemsRef = React.useRef(items);
  itemsRef.current = items;
  const t = useTranslations("components");
  const [result, setResult] = React.useState<{ source: InterceptExecution; error?: string }>();
  React.useEffect(() => {
    if (!source || !loaded) return;
    let cancelled = false;
    void (async () => {
      let current = itemsRef.current;
      let before = current[0]?.seq ?? 0;
      while (!current.some((a) => a.seq === source.seq && a.kind === "tool_use")) {
        const page = await loadPage(before);
        if (cancelled) return;
        if (!page.items.length || (before > 0 && page.items[0].seq >= before)) {
          throw new Error(t("afToolCallGone"));
        }
        mergePage(page);
        current = page.items;
        before = current[0].seq;
        if (!page.hasMore && !current.some((a) => a.seq === source.seq)) {
          throw new Error(t("afToolCallMissing"));
        }
      }
      if (!cancelled) setResult({ source });
    })().catch((error) => {
      if (!cancelled) setResult({ source, error: (error as Error).message });
    });
    return () => {
      cancelled = true;
    };
  }, [source, loaded, loadPage, mergePage, t]);
  return {
    ready: !!source && result?.source === source && !result.error,
    error: result?.source === source ? result?.error : undefined,
  };
}

export function ApprovalExecutionFocus({
  focus,
  history,
}: {
  focus: ReturnType<typeof useApprovalFocus>;
  history: ReturnType<typeof useApprovalHistory>;
}) {
  const { state } = focus;
  const t = useTranslations("components");
  if (!state) return null;
  const error = state.error || history.error;
  let focusStatus: string;
  if (error) {
    focusStatus = t("afLocateFail", { error });
  } else if (history.ready) {
    focusStatus = t("afExpanded", { id: state.id });
  } else {
    focusStatus = t("afLocating", { id: state.id });
  }
  return (
    <div
      role="status"
      className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b px-4 py-2 text-xs"
    >
      <span className={error ? "text-destructive" : "text-muted-foreground"}>{focusStatus}</span>
      <div className="flex gap-2">
        {error ? (
          <Button size="sm" variant="outline" onClick={focus.retry}>
            {t("afRetryLocate")}
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" onClick={focus.close}>
          {t("afCancelLocate")}
        </Button>
      </div>
    </div>
  );
}
