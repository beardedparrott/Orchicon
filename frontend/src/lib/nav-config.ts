import {
  Sparkles,
  LayoutDashboard,
  Activity,
  Coins,
  Folder,
  CheckSquare,
  Container,
  Cpu,
  GitMerge,
  PlayCircle,
  RotateCcw,
  Calendar,
  Repeat,
  Lightbulb,
  CircleCheck,
  ShieldCheck,
  Webhook,
  Plug,
  SlidersHorizontal,
  UserCog,
} from "lucide-react";
export type NavItem = { label: string; to: string; icon: React.ComponentType<{ className?: string }>; iconColor: string; badge?: "NEW"; admin?: boolean; };
export type NavGroup = { label: string; items: NavItem[]; };
export type Domain = "Ask Orchicon" | "Overview" | "Work" | "Execution" | "Automation" | "Enforcement" | "Control";
export const ASK_ORCHICON: NavItem = { label: "Ask Orchicon", to: "/ask-orchicon", icon: Sparkles, iconColor: "text-cyan-700 dark:text-cyan-400" };
export const NAV_GROUPS: NavGroup[] = [
  { label: "Overview", items: [{ label: "Dashboard", to: "/dashboard", icon: LayoutDashboard, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Telemetry", to: "/telemetry", icon: Activity, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Cost Explorer", to: "/cost-explorer", icon: Coins, iconColor: "text-amber-700 dark:text-amber-400" }]},
  { label: "Work", items: [{ label: "Projects", to: "/projects", icon: Folder, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Work Items", to: "/work-items", icon: CheckSquare, iconColor: "text-indigo-700 dark:text-indigo-400" },{ label: "Runtime Images", to: "/runtime-images", icon: Container, iconColor: "text-purple-700 dark:text-purple-400" }]},
  { label: "Execution", items: [{ label: "Workers", to: "/workers", icon: Cpu, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Workflows", to: "/workflows", icon: GitMerge, iconColor: "text-sky-700 dark:text-sky-400" },{ label: "Executions", to: "/executions", icon: PlayCircle, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Recovery", to: "/recovery", icon: RotateCcw, iconColor: "text-rose-700 dark:text-rose-400" }]},
  { label: "Automation", items: [{ label: "Schedules", to: "/schedules", icon: Calendar, iconColor: "text-violet-700 dark:text-violet-400" },{ label: "Recurring Items", to: "/recurring-items", icon: Repeat, iconColor: "text-fuchsia-700 dark:text-fuchsia-400", badge: "NEW" },{ label: "Idea Cloud", to: "/idea-cloud", icon: Lightbulb, iconColor: "text-amber-600 dark:text-amber-400", badge: "NEW" }]},
  { label: "Enforcement", items: [{ label: "Approvals", to: "/approvals", icon: CircleCheck, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Policies", to: "/policies", icon: ShieldCheck, iconColor: "text-indigo-700 dark:text-indigo-400" }]},
  { label: "Control", items: [{ label: "Webhooks", to: "/webhooks", icon: Webhook, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Adapters", to: "/adapters", icon: Plug, iconColor: "text-amber-700 dark:text-amber-400" },{ label: "Settings", to: "/settings", icon: SlidersHorizontal, iconColor: "text-slate-600 dark:text-slate-400" },{ label: "Admin", to: "/admin", icon: UserCog, iconColor: "text-rose-700 dark:text-rose-400", admin: true }]},
];
export function isPathActive(path: string, to: string): boolean { if (path === to) return true; if (path.startsWith(to + "/")) return true; return false; }
export function getActiveDomain(pathname: string): NavGroup | null { if (pathname === ASK_ORCHICON.to || pathname.startsWith(ASK_ORCHICON.to + "/")) return null; for (const group of NAV_GROUPS) { if (group.items.some((it) => isPathActive(pathname, it.to))) return group; } return null; }
export function getActiveItem(pathname: string): NavItem | null { if (isPathActive(pathname, ASK_ORCHICON.to)) return ASK_ORCHICON; let best: NavItem | null = null; let bestLen = -1; for (const group of NAV_GROUPS) { for (const item of group.items) { if (isPathActive(pathname, item.to) && item.to.length > bestLen) { best = item; bestLen = item.to.length; } } } return best; }
export function getBreadcrumbs(pathname: string): { label: string; to?: string }[] { if (isPathActive(pathname, ASK_ORCHICON.to)) { return [{ label: "Ask Orchicon", to: ASK_ORCHICON.to }]; } const domain = getActiveDomain(pathname); const item = getActiveItem(pathname); if (!domain || !item) return []; return [{ label: domain.label },{ label: item.label, to: item.to }]; }
export const ACTIVE_TRIGGER = "nav-active-trigger border";
export const ACTIVE_ITEM = "bg-gradient-to-r from-cyan-500 to-indigo-500 text-white shadow-md";
export const ACTIVE_ITEM_SUBTLE = "nav-active-item border";
export const INACTIVE_ASK = "nav-inactive-ask hover:bg-cyan-500/10";
