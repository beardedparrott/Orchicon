import type { ReactNode } from "react";
import { useEffect, useState } from "react";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { Moon, Sun, Palette, Menu, X } from "lucide-react";

import { cn } from "@/lib/utils";
import { useSession, logout } from "@/auth/auth";
import { setupProactiveRefreshHook, useSessionStore } from "@/auth/session";
import { useThemeStore } from "@/lib/theme-store";

// Application layout shell (docs/10_Frontend_Architecture.md §5).
//
// The shell is a thin client: it renders navigation and server-driven
// content. No business logic lives here (AGENTS.md invariant #1). Nav
// mirrors the API services; routes are added as slices land.

type NavItem = {
  label: string;
  to: string;
  admin?: boolean;
};

const NAV: NavItem[] = [
  { label: "Ask Orchicon", to: "/ask-orchicon" },
  { label: "Dashboard", to: "/dashboard" },
  { label: "Projects", to: "/projects" },
  { label: "Work Items", to: "/work-items" },
  { label: "Schedules", to: "/schedules" },
  { label: "Workers", to: "/workers" },
  { label: "Workflows", to: "/workflows" },
  { label: "Policies", to: "/policies" },
  { label: "Runtime Images", to: "/runtime-images" },
  { label: "Recovery", to: "/recovery" },
  { label: "Executions", to: "/executions" },
  { label: "Approvals", to: "/approvals" },
  { label: "Telemetry + Costs", to: "/telemetry" },
  { label: "Adapters", to: "/adapters" },
  { label: "Webhooks", to: "/webhooks" },
  { label: "Settings", to: "/settings" },
  { label: "Admin", to: "/admin", admin: true },
];

export function AppShell({ children }: { children: ReactNode }) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const isAskOrchicon = path === "/ask-orchicon" || path.startsWith("/ask-orchicon/");
  const session = useSession();
  const loading = useSessionStore((s) => s.loading);
  const navigate = useNavigate();

  // Wire the proactive refresh visibility/focus hook once on mount.
  useEffect(() => {
    setupProactiveRefreshHook();
  }, []);

  // Unauthenticated redirect: once the session has finished loading and no
  // identity resolved, bounce to /login. /login, /signup and /auth/callback
  // are exempt (the login + sign-up pages and the OIDC callback route,
  // which lands here with a token in the fragment before fetchSession
  // completes). This replaces the pre-login ghost dashboard: every
  // protected page requires a real credential (the server 401s without
  // one).
  useEffect(() => {
    if (loading || session.authenticated) return;
    if (path === "/login" || path === "/signup" || path === "/auth/callback") return;
    navigate({ to: "/login" });
  }, [loading, session.authenticated, path, navigate]);

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar />
        <main className={cn("flex-1", isAskOrchicon ? "p-0" : "p-6 lg:p-8")}>
          {children}
        </main>
      </div>
    </div>
  );
}

// Shared nav link list used by the desktop sidebar and the mobile drawer.
function NavLinks() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const session = useSession();
  return (
    <>
      {NAV.filter((item) => !item.admin || session.is_admin).map((item) => {
        const active = path === item.to || path.startsWith(item.to + "/");
        return (
          <Link
            key={item.to}
            to={item.to}
            className={cn(
              "flex items-center rounded-md px-3 py-2 text-sm font-medium transition-colors",
              active
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            )}
          >
            {item.label}
          </Link>
        );
      })}
    </>
  );
}

function Sidebar() {
  return (
    <aside className="hidden w-60 shrink-0 border-r bg-card md:block">
      <div className="flex h-14 items-center gap-2 border-b px-5">
        <span className="text-lg font-semibold tracking-tight">Orchicon</span>
      </div>
      <nav className="space-y-1 p-3">
        <NavLinks />
      </nav>
    </aside>
  );
}

// Mobile-only hamburger drawer. Visible below the md breakpoint where the
// desktop sidebar is hidden, so phone-sized viewports can still navigate.
function MobileNav({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const path = useRouterState({ select: (s) => s.location.pathname });

  // Close the drawer whenever navigation changes (a link was followed).
  useEffect(() => {
    onOpenChange(false);
  }, [path, onOpenChange]);

  // Close on Escape (the drawer is a div, not a native dialog, so it has
  // no built-in Escape behavior).
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, onOpenChange]);

  return (
    <>
      <button
        onClick={() => onOpenChange(!open)}
        className="flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground md:hidden"
        title="Menu"
        aria-label="Toggle navigation menu"
        aria-expanded={open}
      >
        {open ? <X className="h-4 w-4" /> : <Menu className="h-4 w-4" />}
      </button>
      {open && (
        <div
          className="fixed inset-0 z-50 md:hidden"
          role="dialog"
          aria-modal="true"
          aria-label="Navigation"
        >
          <div
            className="absolute inset-0 bg-black/50"
            onClick={() => onOpenChange(false)}
            aria-hidden="true"
          />
          <div className="absolute inset-y-0 left-0 w-60 border-r bg-card shadow-lg">
            <div className="flex h-14 items-center gap-2 border-b px-5">
              <span className="text-lg font-semibold tracking-tight">Orchicon</span>
            </div>
            <nav className="space-y-1 overflow-y-auto p-3">
              <NavLinks />
            </nav>
          </div>
        </div>
      )}
    </>
  );
}

function TopBar() {
  const session = useSession();
  const navigate = useNavigate();
  const toggleMode = useThemeStore((s) => s.toggleMode);
  const mode = useThemeStore((s) => s.mode);
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  if (!session.authenticated) {
    return (
      <header className="flex h-14 items-center justify-between border-b px-6">
        <div className="flex items-center gap-2">
          <MobileNav open={mobileNavOpen} onOpenChange={setMobileNavOpen} />
          <div className="text-sm text-muted-foreground">
            Orchicon control plane · <TopBarVersion />
          </div>
        </div>
        <div className="flex items-center gap-3">
          <ThemeToggleButton mode={mode} onToggle={toggleMode} />
          <Link
            to="/login"
            className="text-xs font-medium text-primary hover:underline"
          >
            Sign in
          </Link>
        </div>
      </header>
    );
  }
  return (
    <header className="flex h-14 items-center justify-between border-b px-6">
      <div className="flex items-center gap-2">
        <MobileNav open={mobileNavOpen} onOpenChange={setMobileNavOpen} />
        <div className="text-sm text-muted-foreground">
          Orchicon control plane · <TopBarVersion />
        </div>
      </div>
      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        <ThemeToggleButton mode={mode} onToggle={toggleMode} />
        <Link
          to="/settings"
          className="flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
          title="Settings"
        >
          <Palette className="h-4 w-4" />
        </Link>
        <span>
          {session.is_admin ? "admin" : "user"} ·{" "}
          <span className="font-mono">
            {session.identity_id?.slice(-8) ?? "—"}
          </span>
        </span>
        <button
          className="text-primary hover:underline"
          onClick={() => {
            logout();
            navigate({ to: "/login" });
          }}
        >
          Sign out
        </button>
      </div>
    </header>
  );
}

function ThemeToggleButton({
  mode,
  onToggle,
}: {
  mode: "light" | "dark";
  onToggle: () => void;
}) {
  return (
    <button
      onClick={onToggle}
      className="flex h-8 w-8 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground"
      title={mode === "dark" ? "Switch to light mode" : "Switch to dark mode"}
    >
      {mode === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
    </button>
  );
}

// TopBarVersion renders the control plane's version (fetched from
// /versionz). Falls back to "dev" while the fetch is in flight or
// failing — the label is non-essential, so we don't block render.
function TopBarVersion() {
  const [version, setVersion] = useState("dev");
  useEffect(() => {
    let cancelled = false;
    fetch("/versionz", { credentials: "include" })
      .then((r) => (r.ok ? r.json() : null))
      .then((body) => {
        if (cancelled) return;
        const v = body && typeof body.version === "string" ? body.version : "";
        if (v) setVersion(v);
      })
      .catch(() => {
        /* leave "dev" */
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return <>{version}</>;
}
