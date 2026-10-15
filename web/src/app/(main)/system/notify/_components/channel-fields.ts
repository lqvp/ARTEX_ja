// Channel field tables and config-value parsing helpers.
//
// This is split from the page because it is **data**, not view code: it
// describes which fields each channel kind has, which control each one uses,
// and how to convert between form text and config values (JSON).
// With it in its own file, adding a channel only means touching this file —
// the page itself never changes.
// Display names for channel kinds. Kept on the frontend because this only
// affects copy; the backend does not need to know. Values are catalog KEYS in
// the `notify` namespace, resolved with t(...) at the render sites (unknown
// kinds fall back to the raw kind string).
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "kindDingtalk",
  feishu: "kindFeishu",
  wecom: "kindWecom",
  webhook: "kindWebhook",
  telegram: "kindTelegram",
  email: "kindEmail",
};

// Config field definitions per channel kind.
//
// We deliberately keep this field table on the frontend instead of letting
// the backend ship a schema: the backend's job is Validate (required/format),
// while the UI needs layout and control types — they care about different
// things. The only coupling point is secret_keys — the backend says which
// fields must render as password inputs, because only each channel
// implementation knows which of its values count as credentials (WeCom's
// entire webhook URL is the credential, while DingTalk's is just one secret
// among several). Forgetting an entry here for a new channel only leaves the
// form blank — it can never fail silently (the hasFields check below says so).
//
// `label`, `help` and `options[].label` hold catalog KEYS in the `notify`
// namespace; consuming components resolve them with t(...) / t.raw(...).
// `placeholder` holds literal display text (URLs / numeric examples), so it
// needs no catalog entry.
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "labelWebhook",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "labelDingtalkSecret",
      kind: "password",
      help: "helpDingtalkSecret",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "labelWebhook",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "labelFeishuSecret", kind: "password", help: "helpFeishuSecret" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "labelWebhook",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "labelTargetUrl", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "labelMethod",
      kind: "select",
      options: [
        { value: "POST", label: "methodPost" },
        { value: "PUT", label: "methodPut" },
        { value: "PATCH", label: "methodPatch" },
        { value: "GET", label: "methodGet" },
      ],
    },
    { key: "headers", label: "labelHeaders", kind: "kv", help: "helpHeaders" },
    {
      key: "body_template",
      label: "labelBodyTemplate",
      kind: "textarea",
      // Value contains literal `{{...}}` braces — must be read with t.raw.
      help: "helpBodyTemplate",
    },
  ],
  telegram: [
    { key: "bot_token", label: "labelBotToken", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "labelChatId", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "labelBaseUrl",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "helpBaseUrl",
    },
  ],
  email: [
    { key: "host", label: "labelSmtpHost", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "labelPort",
      kind: "number",
      placeholder: "587",
      help: "helpPort",
    },
    { key: "username", label: "labelUsername", kind: "text" },
    { key: "password", label: "labelPassword", kind: "password" },
    { key: "from", label: "labelFrom", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "labelTo", kind: "list", help: "helpTo" },
    { key: "tls", label: "labelTls", kind: "switch", help: "helpTls" },
  ],
};

// Severity filter options. `label` holds catalog KEYS (see ChannelFields).
export const SEVERITY_OPTIONS = [
  { value: "", label: "sevAny" },
  { value: "low", label: "sevLow" },
  { value: "medium", label: "sevMedium" },
  { value: "high", label: "sevHigh" },
  { value: "critical", label: "sevCritical" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV parses a textarea of "one KEY=VALUE per line".
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs parses a comma/whitespace-separated list of ids.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords parses a newline/comma-separated keyword list (vulnerability
// type names may contain spaces, so split on newlines or commas only).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
