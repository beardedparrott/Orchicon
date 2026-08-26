import type { ReactNode } from "react";
import { useEffect, useState, useRef } from "react";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import {
  Layers,
  Sparkles,
  ChevronDown,
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
  CircleCheck,
  ShieldCheck,
  Webhook,
  Plug,
  SlidersHorizontal,
  UserCog,
  Bell,
  LogOut,
  Menu,
  X,
  Moon,
  Sun,
} from "lucide-react";

import { cn } from "@/lib/utils";
import { useSession, logout } from "@/auth/auth";
import { setupProactiveRefreshHook, useSessionStore } from "@/auth/session";
import { useThemeStore } from "@/lib/theme-store";

type NavItem = {
  label: string;
  to: string;
  icon: React.ComponentType<{ className?: string }>;
  iconColor: string;
  badge?: "NEW";
  admin?: boolean;
};

type NavGroup = {
  label: string;
  items: NavItem[];
};

const ASK_ORCHICON: NavItem = {
  label: "Ask Orchicon",
  to: "/ask-orchicon",
  icon: Sparkles,
  iconColor: "text-cyan-400",
};

const NAV_GROUPS: NavGroup[] = [
  {
    label: "Overview",
    items: [
      { label: "Dashboard", to: "/dashboard", icon: LayoutDashboard, iconColor: "text-cyan-400" },
      { label: "Telemetry", to: "/telemetry", icon: Activity, iconColor: "text-emerald-400" },
      { label: "Cost Explorer", to: "/cost-explorer", icon: Coins, iconColor: "text-amber-400" },
    ],
  },
  {
    label: "Work",
    items: [
      { label: "Projects", to: "/projects", icon: Folder, iconColor: "text-cyan-400" },
      { label: "Work Items", to: "/work-items", icon: CheckSquare, iconColor: "text-indigo-400" },
      { label: "Runtime Images", to: "/runtime-images", icon: Container, iconColor: "text-purple-400" },
    ],
  },
  {
    label: "Execution",
    items: [
      { label: "Workers", to: "/workers", icon: Cpu, iconColor: "text-cyan-400" },
      { label: "Workflows", to: "/workflows", icon: GitMerge, iconColor: "text-sky-400" },
      { label: "Executions", to: "/executions", icon: PlayCircle, iconColor: "text-emerald-400" },
      { label: "Recovery", to: "/recovery", icon: RotateCcw, iconColor: "text-rose-400" },
    ],
  },
  {
    label: "Automation",
    items: [
      { label: "Schedules", to: "/schedules", icon: Calendar, iconColor: "text-violet-400" },
      { label: "Recurring Items", to: "/recurring-items", icon: Repeat, iconColor: "text-fuchsia-400", badge: "NEW" },
    ],
  },
  {
    label: "Enforcement",
    items: [
      { label: "Approvals", to: "/approvals", icon: CircleCheck, iconColor: "text-emerald-400" },
      { label: "Policies", to: "/policies", icon: ShieldCheck, iconColor: "text-indigo-400" },
    ],
  },
  {
    label: "Control",
    items: [
      { label: "Webhooks", to: "/webhooks", icon: Webhook, iconColor: "text-cyan-400" },
      { label: "Adapters", to: "/adapters", icon: Plug, iconColor: "text-amber-400" },
      { label: "Settings", to: "/settings", icon: SlidersHorizontal, iconColor: "text-slate-400" },
      { label: "Admin", to: "/admin", icon: UserCog, iconColor: "text-rose-400", admin: true },
    ],
  },
];

export function AppShell({ children }: { children: ReactNode }) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const isAskOrchicon = path === "/ask-orchicon" || path.startsWith("/ask-orchicon/");
  const session = useSession();
  const loading = useSessionStore((s) => s.loading);
  const navigate = useNavigate();

  useEffect(() => {
    setupProactiveRefreshHook();
  }, []);

  useEffect(() => {
    if (loading || session.authenticated) return;
    if (path === "/login" || path === "/signup" || path === "/auth/callback") return;
    navigate({ to: "/login" });
  }, [loading, session.authenticated, path, navigate]);

  return (
    <div className="flex min-h-screen flex-col bg-mesh">
      <TopHeader />
      <div className="flex flex-1 overflow-hidden p-4 gap-4">
        <main className={cn("flex-1 min-w-0 overflow-auto flex flex-col")}>
          <div className={cn(isAskOrchicon ? "flex-1 flex flex-col" : "flex-1 p-6 lg:p-8")}>{children}</div>
        </main>
      </div>
    </div>
  );
}

function TopHeader() {
  const session = useSession();
  const [mobileOpen, setMobileOpen] = useState(false);
  const [openGroup, setOpenGroup] = useState<string | null>(null);
  const path = useRouterState({ select: (s) => s.location.pathname });
  useEffect(() => {
    setOpenGroup(null);
  }, [path]);

  return (
    <header className="p-4 pb-0 z-50">
      <div className="max-w-[1920px] mx-auto glass-panel rounded-2xl px-4 py-2 flex items-center justify-between shadow-2xl gap-2">
        <Link to={session.authenticated ? "/ask-orchicon" : "/login"} search={session.authenticated ? { conversationId: null } as never : undefined} className="flex items-center space-x-3 pr-4 border-r border-white/10 shrink-0">
          <div className="w-8 h-8 rounded-lg bg-gradient-to-tr from-cyan-500 to-indigo-500 flex items-center justify-center shadow-lg shadow-cyan-500/30 shrink-0">
            <Layers className="w-5 h-5 text-white" />
          </div>
          <span className="font-bold text-lg tracking-wide text-foreground hidden sm:inline">Orchicon</span>
        </Link>

        <nav className="hidden lg:flex items-center space-x-1 text-sm font-medium flex-1 justify-center min-w-0">
          <Link
            to={ASK_ORCHICON.to}
            search={{ conversationId: null } as never}
            className="flex items-center space-x-2 px-3.5 py-2 rounded-xl text-cyan-400 hover:text-cyan-300 hover:bg-cyan-500/10 transition shrink-0"
          >
            <Sparkles className="w-4 h-4" />
            <span>{ASK_ORCHICON.label}</span>
          </Link>
          {NAV_GROUPS.map((group) => (
            <DropdownGroup
              key={group.label}
              group={group}
              openGroup={openGroup}
              setOpenGroup={setOpenGroup}
            />
          ))}
        </nav>

        <div className="flex lg:hidden items-center ml-auto">
          <button
            onClick={() => setMobileOpen((v) => !v)}
            className="flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            aria-label="Toggle navigation menu"
            aria-expanded={mobileOpen}
          >
            {mobileOpen ? <X className="h-4 w-4" /> : <Menu className="h-4 w-4" />}
          </button>
        </div>

        <div className="flex items-center space-x-1 sm:space-x-3 pl-4 border-l border-white/10 shrink-0">
          {session.authenticated ? (
            <>
              <button
                className="p-2 text-slate-400 hover:text-white rounded-lg hover:bg-white/5 transition relative"
                aria-label="Notifications"
                title="Notifications"
              >
                <Bell className="w-4 h-4" />
                <span className="absolute top-1.5 right-1.5 w-2 h-2 rounded-full bg-cyan-400 animate-pulse" />
              </button>
              <ThemeToggleInline />
              <AvatarBubble />
            </>
          ) : (
            <>
              <ThemeToggleInline />
              <Link to="/login" className="text-xs font-medium text-primary hover:underline px-2">
                Sign in
              </Link>
            </>
          )}
        </div>
      </div>
      {mobileOpen && <MobileDrawer open={mobileOpen} onOpenChange={setMobileOpen} />}
    </header>
  );
}

function ThemeToggleInline() {
  const mode = useThemeStore((s) => s.mode);
  const toggle = useThemeStore((s) => s.toggleMode);
  return (
    <button
      onClick={toggle}
      className="hidden sm:flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
      title={mode === "dark" ? "Switch to light mode" : "Switch to dark mode"}
      aria-label="Toggle theme"
    >
      {mode === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
    </button>
  );
}

function DropdownGroup({
  group,
  openGroup,
  setOpenGroup,
}: {
  group: NavGroup;
  openGroup: string | null;
  setOpenGroup: (v: string | null) => void;
}) {
  const isOpen = openGroup === group.label;
  const containerRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const session = useSession();

  const visibleItems = group.items.filter((it) => !it.admin || session.is_admin);
  if (visibleItems.length === 0) return null;

  useEffect(() => {
    if (!isOpen) return;
    const onPointerDown = (e: PointerEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpenGroup(null);
      }
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpenGroup(null);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [isOpen, setOpenGroup]);

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      setOpenGroup(isOpen ? null : group.label);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setOpenGroup(group.label);
      requestAnimationFrame(() => {
        const first = containerRef.current?.querySelector<HTMLElement>('[role="menuitem"]');
        first?.focus();
      });
    } else if (e.key === "Escape") {
      setOpenGroup(null);
    }
  };

  return (
    <div ref={containerRef} className="relative group">
      <button
        ref={triggerRef}
        aria-haspopup="menu"
        aria-expanded={isOpen}
        aria-label={`${group.label} menu`}
        onClick={() => setOpenGroup(isOpen ? null : group.label)}
        onKeyDown={handleKeyDown}
        className="flex items-center space-x-1.5 px-3.5 py-2 rounded-xl text-muted-foreground hover:text-foreground hover:bg-accent/50 hover:bg-white/5 transition"
      >
        <span>{group.label}</span>
        <ChevronDown
          className={cn(
            "w-3.5 h-3.5 opacity-60 transition-transform",
            isOpen ? "rotate-180" : "group-hover:rotate-180"
          )}
        />
      </button>
      <div
        role="menu"
        aria-label={`${group.label} menu`}
        className={cn(
          group.label === "Control"
            ? "absolute top-full right-0 mt-2 w-52 glass-menu rounded-xl p-1.5 transition-all duration-200 transform z-50"
            : "absolute top-full left-0 mt-2 w-52 glass-menu rounded-xl p-1.5 transition-all duration-200 transform z-50",
          isOpen
            ? "opacity-100 visible translate-y-0"
            : "opacity-0 invisible translate-y-1 group-hover:opacity-100 group-hover:visible group-hover:translate-y-0"
        )}
      >
        {visibleItems.map((item) => {
          const Icon = item.icon;
          return (
            <Link
              key={item.to}
              to={item.to}
              role="menuitem"
              tabIndex={isOpen ? 0 : -1}
              onClick={() => setOpenGroup(null)}
              onKeyDown={(e) => {
                if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                  e.preventDefault();
                  const items = Array.from(containerRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
                  const idx = items.indexOf(e.currentTarget);
                  const next = e.key === "ArrowDown" ? (idx + 1) % items.length : (idx - 1 + items.length) % items.length;
                  items[next]?.focus();
                } else if (e.key === "Escape") {
                  setOpenGroup(null);
                  triggerRef.current?.focus();
                }
              }}
              className="flex items-center justify-between px-3 py-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-accent/50 hover:bg-white/10 transition focus:outline-none focus:bg-white/10 focus:text-white"
            >
              <span className="flex items-center space-x-2.5">
                <Icon className={cn("w-4 h-4", item.iconColor)} />
                <span className="text-sm">{item.label}</span>
              </span>
              {item.badge === "NEW" && (
                <span className="text-[10px] bg-cyan-500/20 text-cyan-300 px-1.5 py-0.5 rounded border border-cyan-500/30 font-semibold">NEW</span>
              )}
            </Link>
          );
        })}
      </div>
    </div>
  );
}

function AvatarBubble() {
  const session = useSession();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  const initial = (session.username?.[0] ?? session.identity_id?.[0] ?? "A").toUpperCase();
  const displayName = session.username ?? (session.identity_id ? `…${session.identity_id.slice(-8)}` : "User");

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  return (
    <div ref={ref} className="relative">
      <button
        ref={triggerRef}
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Profile menu"
        className="w-7 h-7 rounded-full bg-gradient-to-r from-cyan-500 to-blue-600 flex items-center justify-center text-xs font-semibold text-white ring-2 ring-white/10 hover:ring-white/20 transition focus:outline-none focus:ring-cyan-400/50"
      >
        {initial}
      </button>
      <div
        role="menu"
        className={cn(
          "absolute right-0 top-full mt-2 min-w-48 glass-menu rounded-xl p-1.5 shadow-2xl z-50 transition-all duration-200 transform",
          open ? "opacity-100 visible translate-y-0" : "opacity-0 invisible translate-y-1"
        )}
        aria-hidden={!open}
      >
        <div className="px-3 py-2 border-b border-white/10 mb-1">
          <p className="text-xs font-medium text-foreground truncate">{displayName}</p>
          <p className="text-[11px] text-muted-foreground truncate">{session.is_admin ? "Administrator" : "Member"}</p>
        </div>
        <Link
          to="/settings"
          role="menuitem"
          tabIndex={open ? 0 : -1}
          onClick={() => setOpen(false)}
          className="flex items-center space-x-2.5 px-3 py-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-white/10 transition text-sm focus:outline-none focus:bg-white/10"
        >
          <SlidersHorizontal className="w-4 h-4 text-slate-400" />
          <span>Settings</span>
        </Link>
        {session.is_admin && (
          <Link
            to="/admin"
            role="menuitem"
            tabIndex={open ? 0 : -1}
            onClick={() => setOpen(false)}
            className="flex items-center space-x-2.5 px-3 py-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-white/10 transition text-sm focus:outline-none focus:bg-white/10"
          >
            <UserCog className="w-4 h-4 text-rose-400" />
            <span>Admin</span>
          </Link>
        )}
        <button
          role="menuitem"
          tabIndex={open ? 0 : -1}
          onClick={() => {
            setOpen(false);
            logout();
            navigate({ to: "/login" });
          }}
          className="w-full flex items-center space-x-2.5 px-3 py-2 rounded-lg text-muted-foreground hover:text-foreground hover:bg-white/10 transition text-sm focus:outline-none focus:bg-white/10 text-left"
        >
          <LogOut className="w-4 h-4 text-rose-400" />
          <span>Sign out</span>
        </button>
      </div>
    </div>
  );
}

function MobileDrawer({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const session = useSession();

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, onOpenChange]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 lg:hidden" role="dialog" aria-modal="true" aria-label="Navigation">
      <div className="absolute inset-0 bg-black/50" onClick={() => onOpenChange(false)} aria-hidden="true" />
      <div className="absolute inset-y-0 left-0 w-72 glass-menu shadow-lg overflow-y-auto flex flex-col">
        <div className="flex h-14 items-center justify-between gap-2 border-b border-white/10 px-5 shrink-0">
          <span className="flex items-center gap-2 text-lg font-semibold tracking-tight">
            <span className="w-7 h-7 rounded-lg bg-gradient-to-tr from-cyan-500 to-indigo-500 flex items-center justify-center">
              <Layers className="w-4 h-4 text-white" />
            </span>
            Orchicon
          </span>
          <button
            onClick={() => onOpenChange(false)}
            className="p-1 rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            aria-label="Close navigation"
          >
            <X className="h-4 w-4" />
          </button>
        </div>
        <nav className="flex-1 p-3 space-y-4 overflow-y-auto">
          <Link
            to={ASK_ORCHICON.to}
            search={{ conversationId: null } as never}
            onClick={() => onOpenChange(false)}
            className={cn(
              "flex items-center space-x-2.5 px-3 py-2.5 rounded-lg font-medium transition",
              path.startsWith(ASK_ORCHICON.to) ? "bg-cyan-500/10 text-cyan-300 border border-cyan-500/30" : "text-slate-300 hover:bg-white/10 hover:text-white"
            )}
          >
            <Sparkles className="w-4 h-4 text-cyan-400" />
            <span>{ASK_ORCHICON.label}</span>
          </Link>
          {NAV_GROUPS.map((group) => {
            const items = group.items.filter((it) => !it.admin || session.is_admin);
            if (items.length === 0) return null;
            return (
              <div key={group.label}>
                <p className="text-[10px] font-semibold uppercase tracking-wider text-slate-500 px-3 mb-1">{group.label}</p>
                <div className="space-y-1">
                  {items.map((item) => {
                    const Icon = item.icon;
                    const active = path === item.to || path.startsWith(item.to + "/");
                    return (
                      <Link
                        key={item.to}
                        to={item.to}
                        onClick={() => onOpenChange(false)}
                        className={cn(
                          "flex items-center justify-between px-3 py-2 rounded-lg transition text-sm",
                          active ? "bg-white/10 text-foreground border border-[hsla(var(--glass-panel-border)/var(--glass-panel-border-a))]" : "text-slate-300 hover:bg-white/10 hover:text-white"
                        )}
                      >
                        <span className="flex items-center space-x-2.5">
                          <Icon className={cn("w-4 h-4", item.iconColor)} />
                          <span>{item.label}</span>
                        </span>
                        {item.badge === "NEW" && (
                          <span className="text-[10px] bg-cyan-500/20 text-cyan-300 px-1.5 py-0.5 rounded border border-cyan-500/30 font-semibold">NEW</span>
                        )}
                      </Link>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </nav>
      </div>
    </div>
  );
}
