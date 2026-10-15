export const mentionKinds = [
  { kind: "finding", labelKey: "chat.mention.finding", alias: "finding" },
  { kind: "asset", labelKey: "chat.mention.asset", alias: "asset" },
  { kind: "company", labelKey: "chat.mention.company", alias: "company" },
  { kind: "endpoint", labelKey: "chat.mention.endpoint", alias: "endpoint" },
  { kind: "ip", labelKey: "chat.mention.ip", alias: "ip" },
  { kind: "app", labelKey: "chat.mention.app", alias: "app" },
  { kind: "root_domain", labelKey: "chat.mention.rootDomain", alias: "domain" },
  { kind: "subdomain", labelKey: "chat.mention.subdomain", alias: "subdomain" },
  { kind: "service", labelKey: "chat.mention.service", alias: "service" },
] as const;

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    const alias = item.alias;
    if (text === alias || text.startsWith(`${alias} `) || text.startsWith(alias)) {
      return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
    }
  }
  const categories = mentionKinds.filter((item) => item.alias.startsWith(text));
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  const kind = mentionKinds.find((entry) => entry.kind === item.kind)?.alias ?? "asset";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${kind}#${item.id} ${label}]`;
}

// Tokens persisted before the ASCII-alias migration embed the Chinese display label as
// the kind segment; both forms stay matchable so pre-migration history still renders.
const LEGACY_MENTION_LABELS = "漏洞|资产|企业|接口|IP|应用|域名|子域名|服务";
const MENTION_ALIASES = "finding|asset|company|endpoint|ip|app|domain|subdomain|service";
// The kind alternation must stay capturing: group 1 is the kind segment, group 2 the id,
// group 3 the label — selectedMentions interpolates all three into the chip text.
const mentionPattern = new RegExp(
  String.raw`@\[(${MENTION_ALIASES}|${LEGACY_MENTION_LABELS})#([0-9]+)(?: ([^\]\r\n]*))?\]`,
  "g",
);

export function selectedMentions(value: string) {
  return [...value.matchAll(mentionPattern)].map((match) => ({
    token: match[0],
    label: `${match[1]} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`,
    start: match.index,
  }));
}
