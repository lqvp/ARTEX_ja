import {
  Activity,
  Ban,
  BellRing,
  Bot,
  Brain,
  Bug,
  ClipboardList,
  FolderOpen,
  FolderSync,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Plug,
  Radio,
  ScrollText,
  Settings2,
  ShieldAlert,
  Sparkles,
  Target,
  Terminal,
  Wrench,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  titleKey: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  titleKey: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  labelKey?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    labelKey: "group.features",
    items: [
      { id: "dashboard", titleKey: "items.dashboard", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", titleKey: "items.chat", url: "/chat", icon: MessageSquare },
      { id: "tasks", titleKey: "items.tasks", url: "/function/tasks", icon: Target },
      { id: "findings", titleKey: "items.findings", url: "/function/findings", icon: Bug },
      { id: "traffic", titleKey: "items.traffic", url: "/function/traffic", icon: Activity },
      { id: "commands", titleKey: "items.commands", url: "/function/commands", icon: Terminal },
      { id: "llm-records", titleKey: "items.llm-records", url: "/function/llm-records", icon: Radio },
      { id: "assets", titleKey: "items.assets", url: "/function/assets", icon: Network },
      { id: "sync", titleKey: "items.sync", url: "/function/sync", icon: FolderSync },
      { id: "workspace", titleKey: "items.workspace", url: "/function/workspace", icon: FolderOpen },
    ],
  },
  {
    id: 2,
    labelKey: "group.system",
    items: [
      { id: "llm", titleKey: "items.llm", url: "/system/llm", icon: Brain },
      { id: "agents", titleKey: "items.agents", url: "/system/agents", icon: Bot },
      { id: "mcp", titleKey: "items.mcp", url: "/system/mcp", icon: Plug },
      { id: "skills", titleKey: "items.skills", url: "/system/skills", icon: Sparkles },
      { id: "tools", titleKey: "items.tools", url: "/system/tools", icon: Wrench },
      { id: "notify", titleKey: "items.notify", url: "/system/notify", icon: BellRing },
      { id: "intercept", titleKey: "items.intercept", url: "/system/intercept", icon: ShieldAlert },
      { id: "asset-intercept", titleKey: "items.asset-intercept", url: "/system/intercept/assets", icon: Ban },
      { id: "approvals", titleKey: "items.approvals", url: "/system/intercept/approvals", icon: ClipboardList },
      { id: "logs", titleKey: "items.logs", url: "/system/logs", icon: ScrollText },
      { id: "settings", titleKey: "items.settings", url: "/system/settings", icon: Settings2 },
    ],
  },
];
