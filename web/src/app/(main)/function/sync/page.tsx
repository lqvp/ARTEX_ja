"use client";

import * as React from "react";

import { AlertCircleIcon, CheckCircle2Icon, DownloadIcon, PlugZapIcon, RefreshCwIcon, SearchIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { SSProject, SSTask } from "@/lib/types";

type SSStatus = {
  exists: boolean;
  configured: boolean;
  enabled: boolean;
  reachable: boolean;
  url?: string;
  tools: string[];
};

type Dimension = "project" | "task";

// labelKey values are resolved through the sync translator at the render sites
// (checker-visible literal field form).
const ASSET_TYPES: { key: string; labelKey: string }[] = [
  { key: "subdomain", labelKey: "assetTypeSubdomain" },
  { key: "service", labelKey: "assetTypeService" },
  { key: "app", labelKey: "assetTypeApp" },
];

export default function AssetSyncPage() {
  const tsync = useTranslations("sync");
  return (
    <div className="p-4 md:p-6">
      <div className="mb-4">
        <h1 className="font-semibold text-xl">{tsync("title")}</h1>
        <p className="text-muted-foreground text-sm">{tsync("subtitle")}</p>
      </div>
      <Tabs defaultValue="scopesentry">
        <TabsList>
          <TabsTrigger value="scopesentry">ScopeSentry</TabsTrigger>
        </TabsList>
        <TabsContent value="scopesentry" className="mt-4">
          <ScopeSentryPanel />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function ScopeSentryPanel() {
  const tsync = useTranslations("sync");
  const [status, setStatus] = React.useState<SSStatus | null>(null);
  const [loadingStatus, setLoadingStatus] = React.useState(true);

  const loadStatus = React.useCallback(() => {
    setLoadingStatus(true);
    api
      .ssStatus()
      .then(setStatus)
      .catch((e) => toast.error(tsync("toastStatusFail", { message: e.message })))
      .finally(() => setLoadingStatus(false));
  }, [tsync]);

  React.useEffect(() => {
    loadStatus();
  }, [loadStatus]);

  const ready = !!status && status.exists && status.configured && status.enabled;

  return (
    <div className="space-y-4">
      <DataSourceCard status={status} loading={loadingStatus} onChanged={loadStatus} />
      {ready ? (
        <SyncWorkbench />
      ) : (
        <Card>
          <CardContent className="py-8 text-center text-muted-foreground text-sm">
            {tsync("notReadyHint")}
          </CardContent>
        </Card>
      )}
    </div>
  );
}

// ── Data source status card ──────────────────────────────────────────────────

function DataSourceCard({
  status,
  loading,
  onChanged,
}: {
  status: SSStatus | null;
  loading: boolean;
  onChanged: () => void;
}) {
  const tsync = useTranslations("sync");
  const tc = useTranslations("common");
  const [url, setUrl] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  React.useEffect(() => {
    if (status?.url) setUrl(status.url);
  }, [status?.url]);

  const create = async () => {
    setBusy(true);
    try {
      await api.ssDatasource({});
      toast.success(tsync("toastDatasourceCreated"));
      onChanged();
    } catch (e) {
      toast.error(tsync("toastCreateFail", { message: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };

  const save = async () => {
    if (!url.trim()) return toast.error(tsync("toastUrlRequired"));
    setBusy(true);
    try {
      const r = await api.ssDatasource({ url: url.trim(), api_key: apiKey.trim() });
      toast.success(r.enabled ? tsync("toastSavedEnabled") : tsync("toastSavedPending"));
      setApiKey("");
      onChanged();
    } catch (e) {
      toast.error(tsync("toastSaveFail", { message: (e as Error).message }));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="flex items-center gap-2 text-base">
          <PlugZapIcon className="size-4" /> {tsync("datasourceStatus")}
          <StatusBadge status={status} loading={loading} />
        </CardTitle>
        <Button variant="ghost" size="sm" onClick={onChanged} disabled={loading}>
          <RefreshCwIcon className={loading ? "size-4 animate-spin" : "size-4"} /> {tc("refresh")}
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        {!status?.exists ? (
          <div className="flex items-center justify-between gap-4">
            <p className="text-muted-foreground text-sm">{tsync("noDatasourceDesc")}</p>
            <Button onClick={create} disabled={busy}>
              {tsync("createDatasource")}
            </Button>
          </div>
        ) : (
          <>
            {!status.configured && (
              <p className="text-amber-600 text-sm dark:text-amber-500">{tsync("notConfiguredHint")}</p>
            )}
            {status.configured && !status.enabled && (
              <p className="text-amber-600 text-sm dark:text-amber-500">{tsync("notEnabledHint")}</p>
            )}
            <div className="grid gap-3 md:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{tsync("mcpUrlLabel")}</Label>
                <Input
                  placeholder={tsync.raw("mcpUrlPlaceholder")}
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label>{tsync("apiKeyLabel")}</Label>
                <Input
                  type="password"
                  placeholder="ssk_..."
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                />
              </div>
            </div>
            <div className="flex items-center gap-2">
              <Button onClick={save} disabled={busy}>
                {tsync("saveAndEnable")}
              </Button>
              {status.enabled && status.tools.length > 0 && (
                <span className="text-muted-foreground text-xs">{tsync("toolsFound", { n: status.tools.length })}</span>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function StatusBadge({ status, loading }: { status: SSStatus | null; loading: boolean }) {
  const tsync = useTranslations("sync");
  if (loading || !status) return <Badge variant="secondary">{tsync("badgeChecking")}</Badge>;
  if (!status.exists) return <Badge variant="destructive">{tsync("badgeNotCreated")}</Badge>;
  if (!status.configured) return <Badge variant="outline">{tsync("badgeNotConfigured")}</Badge>;
  if (!status.enabled) return <Badge variant="outline">{tsync("badgeNotEnabled")}</Badge>;
  if (status.reachable)
    return (
      <Badge className="bg-emerald-600 hover:bg-emerald-600">
        <CheckCircle2Icon className="mr-1 size-3" /> {tsync("badgeConnected")}
      </Badge>
    );
  return (
    <Badge variant="destructive">
      <AlertCircleIcon className="mr-1 size-3" /> {tsync("badgeUnreachable")}
    </Badge>
  );
}

// ── Sync workbench (project / task dimension) ────────────────────────────────

function SyncWorkbench() {
  const tsync = useTranslations("sync");
  const tc = useTranslations("common");
  const [dimension, setDimension] = React.useState<Dimension>("project");
  const [assetTypes, setAssetTypes] = React.useState<Record<string, boolean>>({
    subdomain: true,
    service: true,
    app: true,
  });
  const [createCompany, setCreateCompany] = React.useState(true);

  const [projects, setProjects] = React.useState<SSProject[]>([]);
  const [tasks, setTasks] = React.useState<SSTask[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [search, setSearch] = React.useState("");
  const [page, setPage] = React.useState(1);
  const [selected, setSelected] = React.useState<Set<string>>(new Set());

  const [syncing, setSyncing] = React.useState(false);
  const [result, setResult] = React.useState<Awaited<ReturnType<typeof api.ssSync>> | null>(null);

  const load = React.useCallback(() => {
    setLoading(true);
    setSelected(new Set());
    const fn =
      dimension === "project"
        ? api.ssProjects(page, 50, search).then((r) => setProjects(r.projects))
        : api.ssTasks(page, 50, search).then(setTasks);
    fn.catch((e) => toast.error(tsync("toastListFail", { message: e.message }))).finally(() => setLoading(false));
  }, [dimension, page, search, tsync]);

  React.useEffect(() => {
    load();
  }, [load]);

  const rows = dimension === "project" ? projects : tasks;
  const idOf = (row: SSProject | SSTask) => (dimension === "project" ? (row as SSProject).id : (row as SSTask).name);

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };
  const toggleAll = () => {
    setSelected((prev) => (prev.size === rows.length ? new Set() : new Set(rows.map(idOf))));
  };

  const chosenTypes = ASSET_TYPES.filter((item) => assetTypes[item.key]).map((item) => item.key);

  const runSync = async () => {
    if (selected.size === 0) {
      return toast.error(dimension === "project" ? tsync("selectProjectFirst") : tsync("selectTaskFirst"));
    }
    if (chosenTypes.length === 0) return toast.error(tsync("selectTypeFirst"));
    setSyncing(true);
    setResult(null);
    try {
      const r = await api.ssSync({
        dimension,
        targets: [...selected],
        asset_types: chosenTypes,
        create_company: dimension === "project" ? createCompany : false,
      });
      setResult(r);
      const total = Object.values(r.synced ?? {}).reduce((a, b) => a + b, 0);
      toast.success(tsync("toastSynced", { n: total }));
    } catch (e) {
      toast.error(tsync("toastSyncFail", { message: (e as Error).message }));
    } finally {
      setSyncing(false);
    }
  };

  const renderRows = () => {
    if (loading) {
      return (
        <TableRow>
          <TableCell colSpan={4} className="py-8 text-center text-muted-foreground text-sm">
            {tc("loading")}
          </TableCell>
        </TableRow>
      );
    }
    if (rows.length === 0) {
      return (
        <TableRow>
          <TableCell colSpan={4} className="py-8 text-center text-muted-foreground text-sm">
            {tsync("noData")}
          </TableCell>
        </TableRow>
      );
    }
    if (dimension === "project") {
      return projects.map((p) => (
        <TableRow key={p.id} className="cursor-pointer" onClick={() => toggle(p.id)}>
          <TableCell onClick={(e) => e.stopPropagation()}>
            <Checkbox checked={selected.has(p.id)} onCheckedChange={() => toggle(p.id)} />
          </TableCell>
          <TableCell className="font-medium">{p.name}</TableCell>
          <TableCell>{p.tag ? <Badge variant="secondary">{p.tag}</Badge> : "—"}</TableCell>
          <TableCell className="text-right">{p.AssetCount ?? 0}</TableCell>
        </TableRow>
      ));
    }
    return tasks.map((task) => (
      <TableRow key={task.id} className="cursor-pointer" onClick={() => toggle(task.name)}>
        <TableCell onClick={(e) => e.stopPropagation()}>
          <Checkbox checked={selected.has(task.name)} onCheckedChange={() => toggle(task.name)} />
        </TableCell>
        <TableCell className="font-medium">{task.name}</TableCell>
        <TableCell>
          <Badge variant={task.progress === 100 ? "secondary" : "outline"}>
            {task.progress != null ? `${task.progress}%` : "—"}
          </Badge>
        </TableCell>
        <TableCell className="text-muted-foreground text-xs">{task.endTime || task.creatTime || "—"}</TableCell>
      </TableRow>
    ));
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{tsync("selectSync")}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Dimension switch */}
        <Tabs
          value={dimension}
          onValueChange={(v) => {
            setDimension(v as Dimension);
            setPage(1);
          }}
        >
          <TabsList>
            <TabsTrigger value="project">{tsync("dimensionProject")}</TabsTrigger>
            <TabsTrigger value="task">{tsync("dimensionTask")}</TabsTrigger>
          </TabsList>
        </Tabs>

        {/* Asset types + options */}
        <div className="flex flex-wrap items-center gap-4">
          <span className="font-medium text-sm">{tsync("syncAssetsLabel")}</span>
          {ASSET_TYPES.map((item) => (
            <label key={item.key} htmlFor={`at-${item.key}`} className="flex items-center gap-1.5 text-sm">
              <Checkbox
                id={`at-${item.key}`}
                checked={!!assetTypes[item.key]}
                onCheckedChange={(c) => setAssetTypes((prev) => ({ ...prev, [item.key]: !!c }))}
              />
              {tsync(item.labelKey)}
            </label>
          ))}
          {dimension === "project" && (
            <label htmlFor="create-company" className="flex items-center gap-1.5 text-sm">
              <Checkbox id="create-company" checked={createCompany} onCheckedChange={(c) => setCreateCompany(!!c)} />
              {tsync("createCompanyLabel")}
            </label>
          )}
        </div>

        {/* Search + actions */}
        <div className="flex items-center gap-2">
          <div className="relative max-w-xs flex-1">
            <SearchIcon className="absolute top-1/2 left-2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="pl-8"
              placeholder={dimension === "project" ? tsync("searchProjectPlaceholder") : tsync("searchTaskPlaceholder")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  setPage(1);
                  load();
                }
              }}
            />
          </div>
          <Button variant="outline" size="sm" onClick={load} disabled={loading}>
            <RefreshCwIcon className={loading ? "size-4 animate-spin" : "size-4"} />
          </Button>
          <div className="flex-1" />
          <span className="text-muted-foreground text-xs">{tsync("selectedCount", { n: selected.size })}</span>
          <Button onClick={runSync} disabled={syncing || selected.size === 0}>
            <DownloadIcon className={syncing ? "size-4 animate-pulse" : "size-4"} /> {tsync("syncSelected")}
          </Button>
        </div>

        {/* List */}
        <div className="rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox checked={rows.length > 0 && selected.size === rows.length} onCheckedChange={toggleAll} />
                </TableHead>
                <TableHead>
                  {dimension === "project" ? tsync("colProjectName") : tsync("colTaskName")}
                </TableHead>
                {dimension === "project" ? (
                  <>
                    <TableHead>{tsync("colTag")}</TableHead>
                    <TableHead className="text-right">{tsync("colAssetCount")}</TableHead>
                  </>
                ) : (
                  <>
                    <TableHead>{tsync("colStatus")}</TableHead>
                    <TableHead>{tsync("colTime")}</TableHead>
                  </>
                )}
              </TableRow>
            </TableHeader>
            <TableBody>{renderRows()}</TableBody>
          </Table>
        </div>

        {/* Pagination */}
        <div className="flex items-center justify-end gap-2">
          <Button variant="outline" size="sm" disabled={page <= 1 || loading} onClick={() => setPage((p) => p - 1)}>
            {tsync("prevPage")}
          </Button>
          <span className="text-muted-foreground text-xs">{tsync("pageN", { page })}</span>
          <Button
            variant="outline"
            size="sm"
            disabled={rows.length < 50 || loading}
            onClick={() => setPage((p) => p + 1)}
          >
            {tsync("nextPage")}
          </Button>
        </div>

        {/* Result */}
        {result && <SyncResult result={result} />}
      </CardContent>
    </Card>
  );
}

function SyncResult({ result }: { result: Awaited<ReturnType<typeof api.ssSync>> }) {
  const tsync = useTranslations("sync");
  const synced = result.synced ?? {};
  // Asset-type labels for the synced counters. An if/else chain with literal t()
  // calls keeps every key statically resolvable (the checker cannot see Record
  // values whose property names do not match); unknown kinds fall through to the
  // raw key, as before.
  const typeLabel = (k: string): string => {
    if (k === "subdomain") return tsync("assetTypeSubdomain");
    if (k === "service") return tsync("assetTypeService");
    if (k === "app") return tsync("assetTypeApp");
    if (k === "ip") return tsync("assetTypeIp");
    return k;
  };
  return (
    <div className="space-y-2 rounded-md border bg-muted/40 p-3 text-sm">
      <div className="flex flex-wrap gap-3">
        {Object.entries(synced).map(([k, v]) => (
          <Badge key={k} variant="secondary">
            {typeLabel(k)}: {v}
          </Badge>
        ))}
      </div>
      {result.companies && result.companies.length > 0 && (
        <p className="text-muted-foreground">{tsync("companiesUpdated", { names: result.companies.join("、") })}</p>
      )}
      {result.warnings && result.warnings.length > 0 && (
        <ul className="list-inside list-disc text-amber-600 dark:text-amber-500">
          {result.warnings.map((wm) => (
            <li key={wm}>{wm}</li>
          ))}
        </ul>
      )}
      {result.errors && result.errors.length > 0 && (
        <ul className="list-inside list-disc text-destructive">
          {result.errors.slice(0, 20).map((em) => (
            <li key={em}>{em}</li>
          ))}
          {result.errors.length > 20 && <li>{tsync("errorsTotal", { n: result.errors.length })}</li>}
        </ul>
      )}
    </div>
  );
}
