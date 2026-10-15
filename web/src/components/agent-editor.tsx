"use client";

import * as React from "react";

import { EyeIcon, GitCompareIcon, InfoIcon, PencilIcon, RotateCcwIcon, SaveIcon, Trash2Icon, XIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Markdown } from "@/components/markdown";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { Agent, AgentDetail, AgentTrigger, MCPServer, PromptVar, PromptVersion, Settings, SkillItem, Tool } from "@/lib/types";

// Traffic tools are host tools gated by the global traffic-capture switch: bindable, but
// only usable when capture is on. Keep this list in sync with traffic.SeedToolMetas.
const TRAFFIC_TOOL_KEYS = new Set(["traffic_search", "traffic_get"]);

// AgentEditor is the tabbed editor for one agent, used inside the agents-page
// drawer (and reused full-page for deep links). Tabs: config + prompt / MCP /
// Skill / Tools. Config + prompt save as before; visibility + tool bindings
// toggle live.
export function AgentEditor({ agentKey, onSaved }: { agentKey: string; onSaved?: () => void }) {
  const t = useTranslations("components");
  const tc = useTranslations("common");
  const [detail, setDetail] = React.useState<AgentDetail | null>(null);
  const [versions, setVersions] = React.useState<PromptVersion[]>([]);
  const [variables, setVariables] = React.useState<PromptVar[]>([]);
  const [mcp, setMcp] = React.useState<MCPServer[]>([]);
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [viewVer, setViewVer] = React.useState<PromptVersion | null>(null);
  const [diffVer, setDiffVer] = React.useState<PromptVersion | null>(null);

  const [prompt, setPrompt] = React.useState("");
  const [mcpVisible, setMcpVisible] = React.useState<number[]>([]);
  const [skillVisible, setSkillVisible] = React.useState<string[]>([]);
  const [preview, setPreview] = React.useState("");
  const [maxTurns, setMaxTurns] = React.useState("0");
  const [runSecs, setRunSecs] = React.useState("600");
  // "" = follow (unbound); otherwise the profile id as a string
  const [llmProfileId, setLlmProfileId] = React.useState("");
  const [llmProfiles, setLlmProfiles] = React.useState<NonNullable<AgentDetail["llm_profiles"]>>([]);
  const [webSearch, setWebSearch] = React.useState(false);
  const [interactiveShell, setInteractiveShell] = React.useState(false);
  const [wrapup, setWrapup] = React.useState("");
  const [wrapupDefault, setWrapupDefault] = React.useState("");
  const [wrapupTurns, setWrapupTurns] = React.useState("0");
  const [wrapupTurnsDefault, setWrapupTurnsDefault] = React.useState(5);
  // Task-level timeout wrapup words (worker/planner only)
  const [ttSupported, setTtSupported] = React.useState(false);
  const [ttWrapup, setTtWrapup] = React.useState("");
  const [ttWrapupDefault, setTtWrapupDefault] = React.useState("");
  const [ttTurns, setTtTurns] = React.useState("0");
  const [ttTurnsDefault, setTtTurnsDefault] = React.useState(5);
  const [settings, setSettings] = React.useState<Settings | null>(null);

  React.useEffect(() => {
    api.mcpServers().then(setMcp).catch(() => {});
    api.skills().then(setSkills).catch(() => {});
    api.tools().then(setTools).catch(() => {});
    api.settings().then(setSettings).catch(() => {});
  }, []);
  // global gates: traffic tools need traffic capture, web search needs the master switch.
  const captureOn = !!settings?.traffic_capture;
  const webSearchGlobalOn = !!settings?.web_search_enabled;

  const reload = React.useCallback(() => {
    api
      .getAgent(agentKey)
      .then((d) => {
        setDetail(d);
        setPrompt(d.prompt ?? "");
        setVariables(d.variables ?? []);
        setVersions(d.versions ?? []);
        setMcpVisible(d.visibility?.mcp ?? []);
        setSkillVisible(d.visibility?.skill ?? []);
        setMaxTurns(String(d.agent?.max_turns ?? 0));
        setRunSecs(String(d.agent?.run_seconds ?? 600));
        setLlmProfileId(d.agent?.llm_profile_id != null ? String(d.agent.llm_profile_id) : "");
        setLlmProfiles(d.llm_profiles ?? []);
        setWebSearch(!!d.agent?.web_search);
        setInteractiveShell(!!d.agent?.interactive_shell);
        setWrapup(d.wrapup_prompt ?? "");
        setWrapupDefault(d.wrapup_default ?? "");
        setWrapupTurns(String(d.wrapup_max_turns ?? 0));
        setWrapupTurnsDefault(d.wrapup_max_turns_default ?? 5);
        setTtSupported(!!d.task_timeout_wrapup_supported);
        setTtWrapup(d.task_timeout_wrapup_prompt ?? "");
        setTtWrapupDefault(d.task_timeout_wrapup_default ?? "");
        setTtTurns(String(d.task_timeout_wrapup_max_turns ?? 0));
        setTtTurnsDefault(d.task_timeout_wrapup_max_turns_default ?? 5);
      })
      .catch(() => setDetail(null))
      .finally(() => setLoaded(true));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);

  async function doPreview() {
    try {
      const r = await api.previewAgentPrompt(agentKey, prompt);
      setPreview(r.error ? t("renderErrorPrefix") + r.error : r.rendered);
    } catch (e) {
      setPreview(t("previewFailPrefix") + (e as Error).message);
    }
  }
  async function savePrompt() {
    try {
      const r = await api.saveAgentPrompt(agentKey, prompt);
      toast.success(t("savedAsVersion", { version: r.version }));
      reload();
      onSaved?.();
    } catch (e) {
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  async function resetPrompt() {
    try {
      const r = await api.resetAgentPrompt(agentKey);
      toast.success(t("resetDoneVersion", { version: r.version }));
      reload();
    } catch (e) {
      toast.error(t("resetFailPrefix") + (e as Error).message);
    }
  }
  async function saveWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(wrapupTurns) || 0));
      await api.saveAgentWrapup(agentKey, wrapup, turns);
      toast.success(wrapup.trim() || turns > 0 ? t("wrapupSaved") : t("wrapupCleared"));
      reload();
    } catch (e) {
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  async function resetWrapup() {
    try {
      await api.resetAgentWrapup(agentKey);
      toast.success(t("resetDone"));
      reload();
    } catch (e) {
      toast.error(t("resetFailPrefix") + (e as Error).message);
    }
  }
  async function saveTaskTimeoutWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(ttTurns) || 0));
      await api.saveAgentTaskTimeoutWrapup(agentKey, ttWrapup, turns);
      toast.success(t("ttWrapupSaved"));
      reload();
    } catch (e) {
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  async function resetTaskTimeoutWrapup() {
    try {
      await api.resetAgentTaskTimeoutWrapup(agentKey);
      toast.success(t("resetDone"));
      reload();
    } catch (e) {
      toast.error(t("resetFailPrefix") + (e as Error).message);
    }
  }
  async function saveConfig() {
    try {
      // Only submit the fields this agent actually shows, so hidden fields (such as
      // the goals decomposer's max_turns) are not overwritten with defaults.
      const patch: Parameters<typeof api.saveAgentConfig>[1] = {
        llm_profile_id: llmProfileId === "" ? null : Number(llmProfileId),
      };
      if (showConfig) {
        patch.max_turns = Math.max(0, Math.floor(Number(maxTurns) || 0));
        patch.run_seconds = Math.max(0, Math.floor(Number(runSecs) || 0));
      }
      if (showWebSearch) patch.web_search = webSearch;
      if (showInteractiveShell) patch.interactive_shell = interactiveShell;
      await api.saveAgentConfig(agentKey, patch);
      toast.success(t("configSaved"));
      reload();
    } catch (e) {
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  // applyVis optimistically updates, persists, and toasts success/failure. On
  // failure it reverts to the prior selection so the UI never lies about state.
  async function applyVis(nextMcp: number[], nextSkill: string[], okMsg: string) {
    const prevMcp = mcpVisible;
    const prevSkill = skillVisible;
    setMcpVisible(nextMcp);
    setSkillVisible(nextSkill);
    try {
      await api.setAgentVisibility(agentKey, nextMcp, nextSkill);
      toast.success(okMsg);
      onSaved?.(); // refresh the list so the card's MCP/Skill counts stay in sync
    } catch (e) {
      setMcpVisible(prevMcp);
      setSkillVisible(prevSkill);
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  function toggleMcp(id: number) {
    const on = mcpVisible.includes(id);
    const name = mcp.find((m) => m.id === id)?.name ?? String(id);
    applyVis(
      on ? mcpVisible.filter((x) => x !== id) : [...mcpVisible, id],
      skillVisible,
      on ? t("mcpHidden", { name }) : t("mcpShown", { name }),
    );
  }
  function toggleSkill(name: string) {
    const on = skillVisible.includes(name);
    applyVis(
      mcpVisible,
      on ? skillVisible.filter((x) => x !== name) : [...skillVisible, name],
      on ? t("skillHidden", { name }) : t("skillShown", { name }),
    );
  }
  async function toggleTool(tool: Tool) {
    const on = tool.agents.includes(agentKey);
    const nextAgents = on ? tool.agents.filter((k) => k !== agentKey) : [...tool.agents, agentKey];
    // optimistic update
    setTools((ts) => ts.map((x) => (x.key === tool.key ? { ...x, agents: nextAgents } : x)));
    try {
      await api.saveTool(tool.key, {
        description: tool.description,
        schema: tool.schema,
        agents: nextAgents,
        enabled: tool.enabled,
      });
      toast.success(on ? t("toolUnbound", { key: tool.key }) : t("toolBound", { key: tool.key }));
      onSaved?.(); // refresh the list so the card's tool count stays in sync
    } catch (e) {
      toast.error(t("toolBindFailPrefix") + (e as Error).message);
      reload();
      api.tools().then(setTools).catch(() => {});
    }
  }

  if (loaded && !detail) {
    return <div className="text-muted-foreground p-6 text-center text-sm">{t("notFound", { key: agentKey })}</div>;
  }
  // config is meaningless for the conversational main agent and the fixed-budget
  // goals decomposer; every other agent (workers, custom assistants) honors it.
  const showConfig = agentKey !== "mainagent" && agentKey !== "goals";
  // web search applies to every conversational/executing agent except the one-shot
  // goals decomposer; it's gated by the global master switch.
  const showWebSearch = agentKey !== "goals";
  // the interactive shell (persistent PTY session tool family) is likewise open to
  // every agent except goals; there is no global gate for it.
  const showInteractiveShell = agentKey !== "goals";
  // every agent (goals/mainagent included) runs on some LLM, so the "default model"
  // binding is open to all of them.
  const showLLM = true;
  // triggers (P3) only attach to custom agents.
  const isCustom = !!detail && !detail.agent?.builtin;

  return (
    <Tabs defaultValue="prompt" className="flex min-h-0 flex-1 flex-col">
      <TabsList className="mx-4 mt-2 w-fit">
        <TabsTrigger value="prompt">{t("tabPrompt")}</TabsTrigger>
        <TabsTrigger value="wrapup">{t("tabWrapup")}</TabsTrigger>
        <TabsTrigger value="mcp">MCP</TabsTrigger>
        <TabsTrigger value="skill">Skill</TabsTrigger>
        <TabsTrigger value="tools">Tools</TabsTrigger>
        {isCustom && <TabsTrigger value="triggers">{t("tabTriggers")}</TabsTrigger>}
      </TabsList>

      {/* config + prompt */}
      <TabsContent value="prompt" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-4">
          {(showLLM || showConfig || showWebSearch || showInteractiveShell) && (
            <div className="grid gap-3 rounded-md border p-3">
              {showLLM && (
                <div className="grid gap-1.5">
                  <Label htmlFor="llm-profile" className="text-xs">{t("defaultModelLabel")}</Label>
                  <div className="flex flex-wrap items-center gap-3">
                    <Select value={llmProfileId || "__follow__"} onValueChange={(v) => setLlmProfileId(v === "__follow__" ? "" : v)}>
                      <SelectTrigger id="llm-profile" className="h-8 w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="__follow__">{t("followGlobalProfile")}</SelectItem>
                        {llmProfiles.map((p) => (
                          <SelectItem key={p.id} value={String(p.id)}>
                            {p.name}（{p.model}）{p.is_default ? t("defaultSuffix") : ""}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <span className="text-muted-foreground max-w-md text-xs">
                      {t("llmProfileHint")}
                    </span>
                  </div>
                </div>
              )}
              <div className="flex flex-wrap items-end gap-3">
                {showConfig && (
                  <>
                    <div className="grid gap-1.5">
                      <Label htmlFor="max-turns" className="text-xs">{t("maxTurnsLabel")}</Label>
                      <Input id="max-turns" type="number" min={0} className="h-8 w-32"
                        value={maxTurns} onChange={(e) => setMaxTurns(e.target.value)} />
                    </div>
                    <div className="grid gap-1.5">
                      <Label htmlFor="run-seconds" className="text-xs">{t("runSecondsLabel")}</Label>
                      <Input id="run-seconds" type="number" min={0} className="h-8 w-32"
                        value={runSecs} onChange={(e) => setRunSecs(e.target.value)} />
                    </div>
                  </>
                )}
                <Button size="sm" variant="outline" onClick={saveConfig}>
                  <SaveIcon /> {t("saveConfigButton")}
                </Button>
              </div>
              {showWebSearch && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch
                    id="web-search"
                    checked={webSearch}
                    disabled={!webSearchGlobalOn}
                    onCheckedChange={setWebSearch}
                  />
                  <div className="grid gap-0.5">
                    <Label htmlFor="web-search" className="text-sm">{t("webSearchLabel")}</Label>
                    <span className="text-muted-foreground text-xs">
                      {webSearchGlobalOn ? t("webSearchOnHint") : t("webSearchOffHint")}
                    </span>
                  </div>
                </div>
              )}
              {showInteractiveShell && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch
                    id="interactive-shell"
                    checked={interactiveShell}
                    onCheckedChange={setInteractiveShell}
                  />
                  <div className="grid gap-0.5">
                    <Label htmlFor="interactive-shell" className="text-sm">{t("interactiveShellLabel")}</Label>
                    <span className="text-muted-foreground text-xs">
                      {t("interactiveShellHint")}
                    </span>
                  </div>
                </div>
              )}
            </div>
          )}

          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">{t("variablesLabel")}</Label>
            <div className="flex flex-wrap gap-2">
              {variables.map((v) => (
                <Tooltip key={v.name}>
                  <TooltipTrigger asChild>
                    <button type="button" onClick={() => setPrompt((p) => `${p}{{.${v.name}}}`)}
                      className="hover:bg-muted inline-flex items-center gap-1 rounded-md border bg-muted/40 px-2 py-1 font-mono text-xs">
                      {`{{.${v.name}}}`}
                      <Badge variant="secondary" className="px-1 py-0 text-[10px]">{v.source}</Badge>
                    </button>
                  </TooltipTrigger>
                  <TooltipContent className="max-w-xs">
                    <p className="font-medium">{v.description}</p>
                    <p className="text-muted-foreground mt-1">{t("examplePrefix")}{v.example}</p>
                  </TooltipContent>
                </Tooltip>
              ))}
              {variables.length === 0 && <span className="text-muted-foreground text-xs">{t("noVariables")}</span>}
            </div>
          </div>

          <Textarea className="font-mono text-xs" rows={16} value={prompt}
            placeholder={t("promptPlaceholder")} onChange={(e) => setPrompt(e.target.value)} />

          <div className="flex flex-wrap gap-2">
            <Dialog>
              <DialogTrigger asChild>
                <Button variant="outline" size="sm" onClick={doPreview}>
                  <EyeIcon /> {t("previewButton")}
                </Button>
              </DialogTrigger>
              <DialogContent className="sm:max-w-2xl">
                <DialogHeader>
                  <DialogTitle>{t("previewTitle")}</DialogTitle>
                  <DialogDescription>{t("previewDescBefore")}{`{{.Var}}`}{t("previewDescAfter")}</DialogDescription>
                </DialogHeader>
                <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-3">
                  <Markdown text={preview} />
                </div>
              </DialogContent>
            </Dialog>
            <Button size="sm" onClick={savePrompt}>
              <SaveIcon /> {t("saveAsNewVersion")}
            </Button>
            <Button variant="outline" size="sm" onClick={resetPrompt}>
              <RotateCcwIcon /> {t("resetDefault")}
            </Button>
          </div>


          <Separator />
          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">{t("versionHistory")}</Label>
            <ul className="grid gap-1">
              {versions.map((ver, i) => (
                <li key={ver.version} className="flex items-center gap-2 rounded-md px-1 py-0.5 text-xs hover:bg-muted/50">
                  <span className="font-mono shrink-0">v{ver.version}</span>
                  {i === 0 && <Badge variant="secondary" className="px-1.5 py-0 shrink-0">{t("currentBadge")}</Badge>}
                  <span className="text-muted-foreground truncate flex-1">{ver.note}</span>
                  {ver.ts && (
                    <span className="text-muted-foreground/60 shrink-0 tabular-nums">
                      {new Date(ver.ts).toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })}
                    </span>
                  )}
                  <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setViewVer(ver)}>
                    <EyeIcon className="size-3" />
                  </Button>
                  {i > 0 && versions[0] && (
                    <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setDiffVer(ver)}>
                      <GitCompareIcon className="size-3" />
                    </Button>
                  )}
                </li>
              ))}
              {versions.length === 0 && (
                <li className="text-muted-foreground text-xs">{t("noVersions")}</li>
              )}
            </ul>
          </div>

          {/* view-version dialog */}
          <Dialog open={!!viewVer} onOpenChange={(o) => { if (!o) setViewVer(null); }}>
            <DialogContent className="sm:max-w-2xl">
              <DialogHeader>
                <DialogTitle>
                  v{viewVer?.version}
                  {viewVer?.version === versions[0]?.version && (
                    <Badge variant="secondary" className="ml-2 px-1.5 py-0 align-middle">{t("currentBadge")}</Badge>
                  )}
                </DialogTitle>
                <DialogDescription>
                  {viewVer?.note || t("noNote")}
                  {viewVer?.ts && (
                    <span className="ml-2 text-muted-foreground/60">
                      {new Date(viewVer.ts).toLocaleString("zh-CN")}
                    </span>
                  )}
                </DialogDescription>
              </DialogHeader>
              <pre className="bg-muted max-h-[55vh] overflow-auto whitespace-pre-wrap rounded-md p-3 font-mono text-xs">
                {viewVer?.template_text || t("emptyText")}
              </pre>
              <div className="flex gap-2 justify-end">
                {viewVer && viewVer.version !== versions[0]?.version && (
                  <Button variant="outline" size="sm" onClick={() => {
                    if (viewVer) { setDiffVer(viewVer); setViewVer(null); }
                  }}>
                    <GitCompareIcon className="mr-1 size-3.5" /> {t("compareWithCurrent")}
                  </Button>
                )}
                <Button size="sm" onClick={() => {
                  if (viewVer) { setPrompt(viewVer.template_text); setViewVer(null); toast.success(t("loadedToEditor", { version: viewVer.version })); }
                }}>
                  {t("loadToEditor")}
                </Button>
              </div>
            </DialogContent>
          </Dialog>

          {/* version-compare dialog */}
          <Dialog open={!!diffVer} onOpenChange={(o) => { if (!o) setDiffVer(null); }}>
            <DialogContent className="sm:max-w-3xl">
              <DialogHeader>
                <DialogTitle>
                  {t("versionCompare", { from: String(diffVer?.version), to: String(versions[0]?.version) })}
                </DialogTitle>
                <DialogDescription>
                  <span className="inline-flex items-center gap-3 text-xs">
                    <span
                      className="rounded bg-red-500/15 px-1.5 py-0.5 text-red-600 dark:text-red-400"
                    >
                      {t("diffDeleted")}
                    </span>
                    <span
                      className="rounded bg-green-500/15 px-1.5 py-0.5 text-green-600 dark:text-green-400"
                    >
                      {t("diffAdded")}
                    </span>
                  </span>
                </DialogDescription>
              </DialogHeader>
              <DiffView oldText={diffVer?.template_text ?? ""} newText={versions[0]?.template_text ?? ""} />
            </DialogContent>
          </Dialog>
        </div>
      </TabsContent>

      {/* wrapup prompt */}
      <TabsContent value="wrapup" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-3">
          <p className="text-muted-foreground text-xs leading-relaxed">
            {/* t.rich: the pre-batch copy bolded "timeout" and "steps exhausted" with
                real <b> elements; plain t() would drop the markup. Handlers receive
                ReactNode chunks — never .join() them. */}
            {t.rich("wrapupIntro", {
              0: (chunk) => <b>{chunk}</b>,
              1: (chunk) => <b>{chunk}</b>,
            })}
          </p>
          <div className="flex items-center gap-2">
            <Label className="text-xs">{t("wrapupBodyLabel")}</Label>
            {wrapup.trim() ? (
              <Badge variant="secondary" className="px-1.5 py-0">{t("customBadge")}</Badge>
            ) : (
              <Badge variant="outline" className="px-1.5 py-0">{t("builtinDefaultBadge")}</Badge>
            )}
          </div>
          <Textarea
            className="font-mono text-xs"
            rows={10}
            value={wrapup}
            placeholder={wrapupDefault || t("wrapupPlaceholder")}
            onChange={(e) => setWrapup(e.target.value)}
          />
          <div className="grid gap-1.5">
            <Label htmlFor="wrapup-turns" className="text-xs">
              {t("wrapupTurnsLabel", { n: wrapupTurnsDefault })}
            </Label>
            <Input id="wrapup-turns" type="number" min={0} className="h-8 w-32"
              value={wrapupTurns} onChange={(e) => setWrapupTurns(e.target.value)} />
          </div>
          <div className="flex gap-2">
            <Button size="sm" onClick={saveWrapup}>{tc("save")}</Button>
            <Button size="sm" variant="outline" onClick={resetWrapup}>{t("resetDefault")}</Button>
          </div>
          {wrapupDefault && (
            <>
              <Separator />
              <div className="grid gap-1.5">
                <Label className="text-muted-foreground text-xs">{t("builtinDefaultLabel")}</Label>
                <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                  {wrapupDefault}
                </pre>
              </div>
            </>
          )}

          {ttSupported && (
            <>
              <Separator className="my-2" />
              <p className="text-muted-foreground text-xs leading-relaxed">
                {/* t.rich: the pre-batch copy bolded three phrases with real <b>
                    elements; plain t() would drop the markup. Handlers receive
                    ReactNode chunks — never .join() them. */}
                {t.rich("ttWrapupIntro", {
                  0: (chunk) => <b>{chunk}</b>,
                  1: (chunk) => <b>{chunk}</b>,
                  2: (chunk) => <b>{chunk}</b>,
                })}
              </p>
              <div className="flex items-center gap-2">
                <Label className="text-xs">{t("ttWrapupBodyLabel")}</Label>
                {ttWrapup.trim() ? (
                  <Badge variant="secondary" className="px-1.5 py-0">{t("customBadge")}</Badge>
                ) : (
                  <Badge variant="outline" className="px-1.5 py-0">{t("builtinDefaultBadge")}</Badge>
                )}
              </div>
              <Textarea
                className="font-mono text-xs"
                rows={10}
                value={ttWrapup}
                placeholder={ttWrapupDefault || t("ttWrapupPlaceholder")}
                onChange={(e) => setTtWrapup(e.target.value)}
              />
              <div className="grid gap-1.5">
                <Label htmlFor="tt-turns" className="text-xs">
                  {t("ttTurnsLabel", { n: ttTurnsDefault })}
                </Label>
                <Input id="tt-turns" type="number" min={0} className="h-8 w-32"
                  value={ttTurns} onChange={(e) => setTtTurns(e.target.value)} />
              </div>
              <div className="flex gap-2">
                <Button size="sm" onClick={saveTaskTimeoutWrapup}>{tc("save")}</Button>
                <Button size="sm" variant="outline" onClick={resetTaskTimeoutWrapup}>{t("resetDefault")}</Button>
              </div>
              {ttWrapupDefault && (
                <div className="grid gap-1.5">
                  <Label className="text-muted-foreground text-xs">{t("builtinDefaultLabel")}</Label>
                  <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                    {ttWrapupDefault}
                  </pre>
                </div>
              )}
            </>
          )}
        </div>
      </TabsContent>

      {/* MCP visibility */}
      <TabsContent value="mcp" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">{t("mcpVisibilityDesc")}</p>
        <div className="grid gap-2">
          {mcp.map((m) => (
            <label key={m.id} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={mcpVisible.includes(m.id)} onCheckedChange={() => toggleMcp(m.id)} />
              {m.name}
              <span className="text-muted-foreground ml-auto text-xs">{m.transport}</span>
            </label>
          ))}
          {mcp.length === 0 && <span className="text-muted-foreground text-xs">{t("noMcp")}</span>}
        </div>
      </TabsContent>

      {/* Skill visibility */}
      <TabsContent value="skill" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">{t("skillVisibilityDesc")}</p>
        <div className="grid gap-2">
          {skills.map((s) => (
            <label key={s.name} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={skillVisible.includes(s.name)} onCheckedChange={() => toggleSkill(s.name)} />
              <span className="font-mono text-xs">{s.name}</span>
              {s.description && <span className="text-muted-foreground ml-auto truncate text-xs">{s.description}</span>}
            </label>
          ))}
          {skills.length === 0 && <span className="text-muted-foreground text-xs">{t("noSkill")}</span>}
        </div>
      </TabsContent>

      {/* Tools binding */}
      <TabsContent value="tools" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">{t("toolsVisibilityDesc")}</p>
        <div className="grid gap-2">
          {tools.map((tool) => {
            const isTraffic = TRAFFIC_TOOL_KEYS.has(tool.key);
            const gated = isTraffic && !captureOn; // traffic tools need traffic capture on
            return (
              <label
                key={tool.key}
                className={cn(
                  "flex items-center gap-2 rounded-md border p-2 text-sm",
                  gated && "opacity-60",
                )}
              >
                <Checkbox
                  checked={tool.agents.includes(agentKey)}
                  disabled={gated}
                  onCheckedChange={() => toggleTool(tool)}
                />
                <span className="font-mono text-xs">{tool.key}</span>
                {isTraffic && (
                  <Badge variant="secondary" className="px-1 py-0 text-[9px]">{t("trafficBadge")}</Badge>
                )}
                {!tool.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">
                    {t("disabledBadge")}
                  </Badge>
                )}
                {gated ? (
                  <span className="text-muted-foreground ml-auto text-xs">{t("captureRequiredHint")}</span>
                ) : (
                  tool.description && (
                    <span className="text-muted-foreground ml-auto line-clamp-1 max-w-[55%] text-xs">
                      {tool.description}
                    </span>
                  )
                )}
              </label>
            );
          })}
          {tools.length === 0 && <span className="text-muted-foreground text-xs">{t("noTools")}</span>}
        </div>
      </TabsContent>

      {/* triggers (P3, custom agents only) */}
      {isCustom && (
        <TabsContent value="triggers" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
          <AgentTriggersTab agentKey={agentKey} agent={detail?.agent} />
        </TabsContent>
      )}
    </Tabs>
  );
}

// ---------- Diff helpers ----------

type DiffLine = { type: "same" | "add" | "del"; text: string };

function computeDiff(oldText: string, newText: string): DiffLine[] {
  const a = oldText.split("\n");
  const b = newText.split("\n");
  const m = a.length;
  const n = b.length;
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0));
  for (let i = 1; i <= m; i++)
    for (let j = 1; j <= n; j++)
      dp[i][j] = a[i - 1] === b[j - 1] ? dp[i - 1][j - 1] + 1 : Math.max(dp[i - 1][j], dp[i][j - 1]);
  const result: DiffLine[] = [];
  let i = m;
  let j = n;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && a[i - 1] === b[j - 1]) {
      result.unshift({ type: "same", text: a[i - 1] });
      i--;
      j--;
    } else if (j > 0 && (i === 0 || dp[i][j - 1] >= dp[i - 1][j])) {
      result.unshift({ type: "add", text: b[j - 1] });
      j--;
    } else {
      result.unshift({ type: "del", text: a[i - 1] });
      i--;
    }
  }
  return result;
}

function DiffView({ oldText, newText }: { oldText: string; newText: string }) {
  const lines = React.useMemo(() => computeDiff(oldText, newText), [oldText, newText]);
  return (
    <pre className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-2 font-mono text-xs leading-5">
      {lines.map((l, idx) => (
        <div
          key={idx}
          className={cn(
            "whitespace-pre-wrap px-1",
            l.type === "del" && "bg-red-500/15 text-red-700 dark:text-red-400",
            l.type === "add" && "bg-green-500/15 text-green-700 dark:text-green-400",
            l.type === "same" && "text-muted-foreground",
          )}
        >
          <span className="select-none mr-1 opacity-50">{l.type === "del" ? "-" : l.type === "add" ? "+" : " "}</span>
          {l.text}
        </div>
      ))}
    </pre>
  );
}

// AgentTriggersTab manages a custom agent's P3 triggers: list + add + delete.
// Each trigger fires (interval / finding discovered / goal met / task timeout /
// tool call, multi-select) → a new conversation runs in parallel with the base
// user message + auto context appended by the backend.
function AgentTriggersTab({ agentKey, agent }: { agentKey: string; agent?: Agent }) {
  const t = useTranslations("components");
  const tc = useTranslations("common");
  const [triggers, setTriggers] = React.useState<AgentTrigger[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  // Post-trigger policy (per agent); seeded from the agent detail, saved on change.
  const [runMode, setRunMode] = React.useState<"serial" | "parallel">(agent?.trigger_run_mode ?? "serial");
  const [mergeMode, setMergeMode] = React.useState<"by_task" | "all" | "none">(agent?.trigger_merge_mode ?? "all");
  const [maxParallel, setMaxParallel] = React.useState(String(agent?.trigger_max_parallel ?? 5));
  React.useEffect(() => {
    setRunMode(agent?.trigger_run_mode ?? "serial");
    setMergeMode(agent?.trigger_merge_mode ?? "all");
    setMaxParallel(String(agent?.trigger_max_parallel ?? 5));
  }, [agent?.trigger_run_mode, agent?.trigger_merge_mode, agent?.trigger_max_parallel]);

  async function saveBehavior(patch: {
    trigger_run_mode?: "serial" | "parallel";
    trigger_merge_mode?: "by_task" | "all" | "none";
    trigger_max_parallel?: number;
  }) {
    try {
      await api.saveAgentConfig(agentKey, patch);
    } catch (e) {
      toast.error(t("behaviorSaveFailPrefix") + (e as Error).message);
    }
  }
  const [onInterval, setOnInterval] = React.useState(false);
  const [intervalSec, setIntervalSec] = React.useState("60");
  const [onFinding, setOnFinding] = React.useState(false);
  const [onGoalMet, setOnGoalMet] = React.useState(false);
  const [onTaskTimeout, setOnTaskTimeout] = React.useState(false);
  const [onToolCall, setOnToolCall] = React.useState(false);
  const [onTaskCreate, setOnTaskCreate] = React.useState(false);
  const [intervalMsg, setIntervalMsg] = React.useState("");
  const [findingMsg, setFindingMsg] = React.useState("");
  const [goalMsg, setGoalMsg] = React.useState("");
  const [taskTimeoutMsg, setTaskTimeoutMsg] = React.useState("");
  const [toolCallMsg, setToolCallMsg] = React.useState("");
  const [taskCreateMsg, setTaskCreateMsg] = React.useState("");
  const [toolNames, setToolNames] = React.useState<string[]>([]);
  const [saving, setSaving] = React.useState(false);
  // null = add mode; non-null = editing the trigger with that id.
  const [editingId, setEditingId] = React.useState<number | null>(null);

  const reload = React.useCallback(() => {
    api.agentTriggers(agentKey).then(setTriggers).catch(() => setTriggers([]));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);
  React.useEffect(() => {
    api.tools().then(setTools).catch(() => setTools([]));
  }, []);

  function toggleTool(key: string) {
    setToolNames((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  }

  // resetForm clears the form and returns to "add" mode.
  function resetForm() {
    setEditingId(null);
    setOnInterval(false);
    setIntervalSec("60");
    setOnFinding(false);
    setOnGoalMet(false);
    setOnTaskTimeout(false);
    setOnToolCall(false);
    setOnTaskCreate(false);
    setIntervalMsg("");
    setFindingMsg("");
    setGoalMsg("");
    setTaskTimeoutMsg("");
    setToolCallMsg("");
    setTaskCreateMsg("");
    setToolNames([]);
  }

  // startEdit loads an existing trigger into the form and enters "edit" mode.
  function startEdit(trigger: AgentTrigger) {
    setEditingId(trigger.id);
    setOnInterval(trigger.interval_sec > 0);
    setIntervalSec(trigger.interval_sec > 0 ? String(trigger.interval_sec) : "60");
    setOnFinding(trigger.on_finding);
    setOnGoalMet(trigger.on_goal_met);
    setOnTaskTimeout(trigger.on_task_timeout);
    setOnToolCall(trigger.on_tool_call);
    setOnTaskCreate(trigger.on_task_create);
    setIntervalMsg(trigger.interval_message);
    setFindingMsg(trigger.finding_message);
    setGoalMsg(trigger.goal_message);
    setTaskTimeoutMsg(trigger.task_timeout_message);
    setToolCallMsg(trigger.tool_call_message);
    setTaskCreateMsg(trigger.task_create_message);
    setToolNames(trigger.tool_names ?? []);
  }

  // submit follows editingId into "add" or "save changes"; editing keeps that
  // trigger's enabled state.
  async function submit() {
    const n = onInterval ? Math.max(1, Math.floor(Number(intervalSec) || 0)) : 0;
    if (n === 0 && !onFinding && !onGoalMet && !onTaskTimeout && !onToolCall && !onTaskCreate) {
      toast.error(t("needOneCondition"));
      return;
    }
    if (onToolCall && toolNames.length === 0) {
      toast.error(t("needOneTool"));
      return;
    }
    const body = {
      interval_sec: n,
      on_finding: onFinding,
      on_goal_met: onGoalMet,
      on_task_timeout: onTaskTimeout,
      on_tool_call: onToolCall,
      on_task_create: onTaskCreate,
      interval_message: intervalMsg.trim(),
      finding_message: findingMsg.trim(),
      goal_message: goalMsg.trim(),
      task_timeout_message: taskTimeoutMsg.trim(),
      tool_call_message: toolCallMsg.trim(),
      task_create_message: taskCreateMsg.trim(),
      tool_names: onToolCall ? toolNames : [],
    };
    setSaving(true);
    try {
      if (editingId != null) {
        const cur = triggers.find((x) => x.id === editingId);
        await api.updateTrigger(editingId, { ...body, enabled: cur?.enabled ?? true });
        toast.success(t("savedChanges"));
      } else {
        await api.createTrigger(agentKey, { ...body, enabled: true });
        toast.success(t("triggerAdded"));
      }
      resetForm();
      reload();
    } catch (e) {
      toast.error((editingId != null ? t("saveFailPrefix") : t("addFailPrefix")) + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  async function toggleEnabled(trigger: AgentTrigger) {
    try {
      await api.updateTrigger(trigger.id, {
        enabled: !trigger.enabled,
        interval_sec: trigger.interval_sec,
        on_finding: trigger.on_finding,
        on_goal_met: trigger.on_goal_met,
        on_task_timeout: trigger.on_task_timeout,
        on_tool_call: trigger.on_tool_call,
        on_task_create: trigger.on_task_create,
        interval_message: trigger.interval_message,
        finding_message: trigger.finding_message,
        goal_message: trigger.goal_message,
        task_timeout_message: trigger.task_timeout_message,
        tool_call_message: trigger.tool_call_message,
        task_create_message: trigger.task_create_message,
        tool_names: trigger.tool_names,
      });
      reload();
    } catch (e) {
      toast.error(t("saveFailPrefix") + (e as Error).message);
    }
  }
  async function del(id: number) {
    try {
      await api.deleteTrigger(id);
      if (editingId === id) resetForm();
      reload();
    } catch (e) {
      toast.error(t("deleteFailPrefix") + (e as Error).message);
    }
  }

  // condLabel summarises one trigger's conditions as a " · "-joined list.
  function condLabel(trigger: AgentTrigger): string {
    const parts: string[] = [];
    if (trigger.interval_sec > 0) parts.push(t("condInterval", { n: trigger.interval_sec }));
    if (trigger.on_finding) parts.push(t("condFinding"));
    if (trigger.on_goal_met) parts.push(t("condGoalMet"));
    if (trigger.on_task_timeout) parts.push(t("condTaskTimeout"));
    if (trigger.on_tool_call) parts.push(t("condToolCall", { n: trigger.tool_names.length }));
    if (trigger.on_task_create) parts.push(t("condTaskCreate"));
    return parts.join(" · ") || t("condNone");
  }

  return (
    <div className="grid gap-4">
      <p className="text-muted-foreground text-xs">
        {/* t.rich: the pre-batch copy bolded "starts a new session" with a real <b>
            element; plain t() would drop the markup. Handlers receive ReactNode
            chunks — never .join() them. */}
        {t.rich("triggersIntro", {
          0: (chunk) => <b>{chunk}</b>,
        })}
      </p>

      {/* post-trigger policy */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">{t("behaviorLabel")}</Label>
        <div className="flex flex-wrap items-center gap-4">
          <div className="grid gap-1">
            <Label className="text-xs">{t("runModeLabel")}</Label>
            <Select
              value={runMode}
              onValueChange={(v) => {
                const rm = v as "serial" | "parallel";
                setRunMode(rm);
                saveBehavior({ trigger_run_mode: rm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="serial">{t("runModeSerial")}</SelectItem>
                <SelectItem value="parallel">{t("runModeParallel")}</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid gap-1">
            <Label className="text-xs">{t("mergeModeLabel")}</Label>
            <Select
              value={mergeMode}
              disabled={runMode === "parallel"}
              onValueChange={(v) => {
                const mm = v as "by_task" | "all" | "none";
                setMergeMode(mm);
                saveBehavior({ trigger_merge_mode: mm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="by_task">{t("mergeByTask")}</SelectItem>
                <SelectItem value="all">{t("mergeAll")}</SelectItem>
                <SelectItem value="none">{t("mergeNone")}</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {runMode === "parallel" && (
            <div className="grid gap-1">
              <Label htmlFor="tr-maxpar" className="text-xs">{t("maxParallelLabel")}</Label>
              <Input
                id="tr-maxpar"
                type="number"
                min={0}
                className="h-8 w-28"
                value={maxParallel}
                onChange={(e) => setMaxParallel(e.target.value)}
                onBlur={() => {
                  const n = Math.max(0, Math.floor(Number(maxParallel) || 0));
                  setMaxParallel(String(n));
                  saveBehavior({ trigger_max_parallel: n });
                }}
              />
            </div>
          )}
        </div>
        <p className="text-muted-foreground text-xs">
          {runMode === "parallel"
            ? t("descParallel")
            : mergeMode === "by_task"
              ? t("descSerialByTask")
              : mergeMode === "all"
                ? t("descSerialAll")
                : t("descSerialNone")}
        </p>
      </div>

      {/* add / edit trigger */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">
          {editingId != null
            ? t("editingTriggerLabel", { id: editingId })
            : t("newTriggerLabel")}
        </Label>

        {/* interval */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onInterval} onCheckedChange={(v) => setOnInterval(!!v)} /> {t("intervalTrigger")}
          </label>
          {onInterval && (
            <div className="grid gap-1.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="tr-interval" className="text-xs">{t("everyLabel")}</Label>
                <Input id="tr-interval" type="number" min={1} className="h-8 w-24"
                  value={intervalSec} onChange={(e) => setIntervalSec(e.target.value)} />
                <span className="text-muted-foreground text-xs">{t("secondsUnit")}</span>
              </div>
              <Textarea className="text-xs" rows={2} value={intervalMsg}
                placeholder={t("intervalMsgPlaceholder")} onChange={(e) => setIntervalMsg(e.target.value)} />
            </div>
          )}
        </div>

        {/* finding */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onFinding} onCheckedChange={(v) => setOnFinding(!!v)} /> {t("findingTrigger")}
          </label>
          {onFinding && (
            <Textarea className="text-xs" rows={2} value={findingMsg}
              placeholder={t("findingMsgPlaceholder")} onChange={(e) => setFindingMsg(e.target.value)} />
          )}
        </div>

        {/* goal met */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onGoalMet} onCheckedChange={(v) => setOnGoalMet(!!v)} /> {t("goalTrigger")}
          </label>
          {onGoalMet && (
            <Textarea className="text-xs" rows={2} value={goalMsg}
              placeholder={t("goalMsgPlaceholder")} onChange={(e) => setGoalMsg(e.target.value)} />
          )}
        </div>

        {/* task timeout */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskTimeout} onCheckedChange={(v) => setOnTaskTimeout(!!v)} /> {t("timeoutTrigger")}
          </label>
          {onTaskTimeout && (
            <Textarea className="text-xs" rows={2} value={taskTimeoutMsg}
              placeholder={t("taskTimeoutMsgPlaceholder")} onChange={(e) => setTaskTimeoutMsg(e.target.value)} />
          )}
        </div>

        {/* tool call */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onToolCall} onCheckedChange={(v) => setOnToolCall(!!v)} /> {t("toolCallTrigger")}
          </label>
          {onToolCall && (
            <div className="grid gap-1.5">
              <div className="text-muted-foreground text-xs">
                {/* t.rich: the pre-batch copy bolded "completes" with a real <b>
                    element. Handlers receive ReactNode chunks — never .join() them. */}
                {t.rich("toolListenDesc", {
                  n: toolNames.length,
                  0: (chunk) => <b>{chunk}</b>,
                })}
              </div>
              <div className="max-h-40 overflow-y-auto rounded-md border p-2">
                {tools.length === 0 && <span className="text-muted-foreground text-xs">{t("noToolsList")}</span>}
                <div className="grid gap-1">
                  {tools.map((tool) => (
                    <label key={tool.key} className="flex items-start gap-2 text-xs">
                      <Checkbox className="mt-0.5" checked={toolNames.includes(tool.key)}
                        onCheckedChange={() => toggleTool(tool.key)} />
                      <span className="min-w-0">
                        <span className="font-medium">{tool.key}</span>
                        {tool.description && <span className="text-muted-foreground line-clamp-1"> {tool.description}</span>}
                      </span>
                    </label>
                  ))}
                </div>
              </div>
              <Textarea className="text-xs" rows={2} value={toolCallMsg}
                placeholder={t("toolCallMsgPlaceholder")} onChange={(e) => setToolCallMsg(e.target.value)} />
            </div>
          )}
        </div>

        {/* task creation */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskCreate} onCheckedChange={(v) => setOnTaskCreate(!!v)} /> {t("taskCreateTrigger")}
          </label>
          {onTaskCreate && (
            <Textarea className="text-xs" rows={2} value={taskCreateMsg}
              placeholder={t("taskCreateMsgPlaceholder")} onChange={(e) => setTaskCreateMsg(e.target.value)} />
          )}
        </div>

        <div className="flex items-center gap-2">
          <Button size="sm" onClick={submit} disabled={saving}>
            <SaveIcon /> {editingId != null ? t("saveChangesButton") : t("addTriggerButton")}
          </Button>
          {editingId != null && (
            <Button size="sm" variant="ghost" onClick={resetForm} disabled={saving}>
              <XIcon /> {t("cancelEditButton")}
            </Button>
          )}
        </div>
      </div>

      {/* existing triggers */}
      <div className="grid gap-2">
        <Label className="text-muted-foreground text-xs">{t("existingTriggers")}</Label>
        {triggers.length === 0 && <span className="text-muted-foreground text-xs">{t("noTriggers")}</span>}
        {triggers.map((trigger) => (
          <div
            key={trigger.id}
            className={cn(
              "flex items-start gap-2 rounded-md border p-2 text-sm",
              editingId === trigger.id && "border-primary bg-primary/5",
            )}
          >
            <Switch checked={trigger.enabled} onCheckedChange={() => toggleEnabled(trigger)} className="mt-0.5" />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-medium">{condLabel(trigger)}</span>
                {!trigger.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">
                    {t("disabledBadge")}
                  </Badge>
                )}
              </div>
              <div className="text-muted-foreground grid gap-0.5 text-xs">
                {trigger.interval_sec > 0 && trigger.interval_message && (
                  <div className="line-clamp-1">{t("rowPrefixInterval")}{trigger.interval_message}</div>
                )}
                {trigger.on_finding && trigger.finding_message && (
                  <div className="line-clamp-1">{t("rowPrefixFinding")}{trigger.finding_message}</div>
                )}
                {trigger.on_goal_met && trigger.goal_message && (
                  <div className="line-clamp-1">{t("rowPrefixGoal")}{trigger.goal_message}</div>
                )}
                {trigger.on_task_timeout && trigger.task_timeout_message && (
                  <div className="line-clamp-1">{t("rowPrefixTimeout")}{trigger.task_timeout_message}</div>
                )}
                {trigger.on_task_create && trigger.task_create_message && (
                  <div className="line-clamp-1">{t("rowPrefixTaskCreate")}{trigger.task_create_message}</div>
                )}
                {trigger.on_tool_call && (
                  <>
                    <div className="line-clamp-1">
                      {t("rowPrefixTool")}
                      {trigger.tool_names.join("、") || t("noneSelected")}
                    </div>
                    {trigger.tool_call_message && (
                      <div className="line-clamp-1">{t("rowPrefixMessage")}{trigger.tool_call_message}</div>
                    )}
                  </>
                )}
              </div>
            </div>
            <Button variant="ghost" size="icon-sm" className="text-muted-foreground hover:text-foreground"
              onClick={() => startEdit(trigger)} title={tc("edit")}>
              <PencilIcon className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon-sm" className="text-muted-foreground hover:text-destructive"
              onClick={() => del(trigger.id)} title={tc("delete")}>
              <Trash2Icon className="size-3.5" />
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}
