import {
  Activity,
  Bug,
  ClipboardList,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Plus,
  Radio,
  ScrollText,
  Target,
  Terminal,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
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
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    items: [
      { id: "new-task", title: "新建任务", url: "/function/tasks/new", icon: Plus },
      { id: "tasks", title: "任务", url: "/function/tasks", icon: Target },
      { id: "findings", title: "发现", url: "/function/findings", icon: Bug },
      { id: "assets", title: "资产", url: "/function/assets", icon: Network },
      { id: "chat", title: "对话", url: "/chat", icon: MessageSquare },
      { id: "approvals", title: "审批", url: "/system/intercept/approvals", icon: ClipboardList },
    ],
  },
  {
    id: 2,
    label: "运行记录",
    items: [
      { id: "dashboard", title: "仪表盘", url: "/dashboard", icon: LayoutDashboard },
      { id: "traffic", title: "流量", url: "/function/traffic", icon: Activity },
      { id: "commands", title: "工具执行", url: "/function/commands", icon: Terminal },
      { id: "llm-records", title: "LLM 录制", url: "/function/llm-records", icon: Radio },
      { id: "logs", title: "日志", url: "/system/logs", icon: ScrollText },
    ],
  },
];
