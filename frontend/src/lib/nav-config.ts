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
  Server,
} from "lucide-react";
// `desktopOnly` marks an entry that exists ONLY in the Wails desktop shell.
//
// THE FLAG IS HONOURED AT THE NAV, NOT INSIDE THE PAGE: a browser must not render
// the entry at all — no item, no disabled item, no notice explaining its absence.
// Absence is the requirement (Plane Lifecycle Control AC4). Every consumer of
// NAV_GROUPS goes through isItemVisible/visibleNavGroups so the flag cannot be
// honoured in one place and forgotten in another, which is how the entry would
// leak into the browser through a second path.
export type NavItem = { label: string; to: string; icon: React.ComponentType<{ className?: string }>; iconColor: string; badge?: "NEW"; admin?: boolean; desktopOnly?: boolean; };
export type NavGroup = { label: string; items: NavItem[]; };
export type Domain = "Ask Orchicon" | "Overview" | "Work" | "Execution" | "Automation" | "Enforcement" | "Control";
export const ASK_ORCHICON: NavItem = { label: "Ask Orchicon", to: "/ask-orchicon", icon: Sparkles, iconColor: "text-cyan-700 dark:text-cyan-400" };
export const NAV_GROUPS: NavGroup[] = [
  { label: "Overview", items: [{ label: "Dashboard", to: "/dashboard", icon: LayoutDashboard, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Telemetry", to: "/telemetry", icon: Activity, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Cost Explorer", to: "/cost-explorer", icon: Coins, iconColor: "text-amber-700 dark:text-amber-400" }]},
  { label: "Work", items: [{ label: "Projects", to: "/projects", icon: Folder, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Work Items", to: "/work-items", icon: CheckSquare, iconColor: "text-indigo-700 dark:text-indigo-400" },{ label: "Runtime Images", to: "/runtime-images", icon: Container, iconColor: "text-purple-700 dark:text-purple-400" }]},
  { label: "Execution", items: [{ label: "Workers", to: "/workers", icon: Cpu, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Workflows", to: "/workflows", icon: GitMerge, iconColor: "text-sky-700 dark:text-sky-400" },{ label: "Executions", to: "/executions", icon: PlayCircle, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Schedules", to: "/schedules", icon: Calendar, iconColor: "text-violet-700 dark:text-violet-400" },{ label: "Recovery", to: "/recovery", icon: RotateCcw, iconColor: "text-rose-700 dark:text-rose-400" }]},
  { label: "Automation", items: [{ label: "Recurring Items", to: "/recurring-items", icon: Repeat, iconColor: "text-fuchsia-700 dark:text-fuchsia-400", badge: "NEW" },{ label: "Idea Cloud", to: "/idea-cloud", icon: Lightbulb, iconColor: "text-amber-600 dark:text-amber-400", badge: "NEW" }]},
  { label: "Enforcement", items: [{ label: "Approvals", to: "/approvals", icon: CircleCheck, iconColor: "text-emerald-700 dark:text-emerald-400" },{ label: "Policies", to: "/policies", icon: ShieldCheck, iconColor: "text-indigo-700 dark:text-indigo-400" }]},
  { label: "Control", items: [{ label: "Webhooks", to: "/webhooks", icon: Webhook, iconColor: "text-cyan-700 dark:text-cyan-400" },{ label: "Adapters", to: "/adapters", icon: Plug, iconColor: "text-amber-700 dark:text-amber-400" },{ label: "Settings", to: "/settings", icon: SlidersHorizontal, iconColor: "text-slate-600 dark:text-slate-400" },{ label: "Manage Server", to: "/manage-server", icon: Server, iconColor: "text-cyan-700 dark:text-cyan-400", desktopOnly: true },{ label: "Admin", to: "/admin", icon: UserCog, iconColor: "text-rose-700 dark:text-rose-400", admin: true }]},
];
// THE ONLY WAY TO ENUMERATE THE NAV. `desktop` is the single desktop signal
// (lib/desktop.ts:isDesktop). A consumer that iterates NAV_GROUPS directly is a
// bug: it would render a desktopOnly entry in a browser.
export function isItemVisible(item: NavItem, desktop: boolean): boolean { return !item.desktopOnly || desktop; }
export function visibleNavGroups(desktop: boolean): NavGroup[] { return NAV_GROUPS.map((g) => ({ ...g, items: g.items.filter((it) => isItemVisible(it, desktop)) })).filter((g) => g.items.length > 0); }
export function isPathActive(path: string, to: string): boolean { if (path === to) return true; if (path.startsWith(to + "/")) return true; return false; }
// `desktop` is REQUIRED, not defaulted: a forgotten call site must be a typecheck
// failure rather than a silent browser leak, and a filtered-out item must never
// resolve as active or as a breadcrumb.
export function getActiveDomain(pathname: string, desktop: boolean): NavGroup | null { if (pathname === ASK_ORCHICON.to || pathname.startsWith(ASK_ORCHICON.to + "/")) return null; for (const group of visibleNavGroups(desktop)) { if (group.items.some((it) => isPathActive(pathname, it.to))) return group; } return null; }
export function getActiveItem(pathname: string, desktop: boolean): NavItem | null { if (isPathActive(pathname, ASK_ORCHICON.to)) return ASK_ORCHICON; let best: NavItem | null = null; let bestLen = -1; for (const group of visibleNavGroups(desktop)) { for (const item of group.items) { if (isPathActive(pathname, item.to) && item.to.length > bestLen) { best = item; bestLen = item.to.length; } } } return best; }
export function getBreadcrumbs(pathname: string, desktop: boolean): { label: string; to?: string }[] { if (isPathActive(pathname, ASK_ORCHICON.to)) { return [{ label: "Ask Orchicon", to: ASK_ORCHICON.to }]; } const domain = getActiveDomain(pathname, desktop); const item = getActiveItem(pathname, desktop); if (!domain || !item) return []; return [{ label: domain.label },{ label: item.label, to: item.to }]; }
export const ACTIVE_TRIGGER = "nav-active-trigger border";
export const ACTIVE_ITEM = "bg-gradient-to-r from-cyan-500 to-indigo-500 text-white shadow-md";
export const ACTIVE_ITEM_SUBTLE = "nav-active-item border";
export const INACTIVE_ASK = "nav-inactive-ask hover:bg-cyan-500/10";
