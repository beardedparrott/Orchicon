import type { ReactNode } from "react";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";

import { cn } from "@/lib/utils";
import { useSession, logout } from "@/auth/auth";

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
  { label: "Dashboard", to: "/" },
  { label: "Projects", to: "/projects" },
  { label: "Work Items", to: "/work-items" },
  { label: "Workers", to: "/workers" },
  { label: "Workflows", to: "/workflows" },
  { label: "Policies", to: "/policies" },
  { label: "Recovery", to: "/recovery" },
  { label: "Executions", to: "/executions" },
  { label: "Telemetry", to: "/telemetry" },
  { label: "Adapters", to: "/adapters" },
  { label: "Webhooks", to: "/webhooks" },
  { label: "Admin", to: "/admin", admin: true },
];

export function AppShell({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <div className="flex flex-1 flex-col">
        <TopBar />
        <main className="flex-1 p-6 lg:p-8">{children}</main>
      </div>
    </div>
  );
}

function Sidebar() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const session = useSession();
  return (
    <aside className="hidden w-60 shrink-0 border-r bg-card md:block">
      <div className="flex h-14 items-center gap-2 border-b px-5">
        <span className="text-lg font-semibold tracking-tight">Orchicon</span>
      </div>
      <nav className="space-y-1 p-3">
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
      </nav>
    </aside>
  );
}

function TopBar() {
  const session = useSession();
  const navigate = useNavigate();
  if (!session.authenticated) {
    return (
      <header className="flex h-14 items-center justify-between border-b px-6">
        <div className="text-sm text-muted-foreground">
          Orchicon control plane · v0.1
        </div>
        <Link
          to="/login"
          className="text-xs font-medium text-primary hover:underline"
        >
          Sign in
        </Link>
      </header>
    );
  }
  return (
    <header className="flex h-14 items-center justify-between border-b px-6">
      <div className="text-sm text-muted-foreground">
        Orchicon control plane · v0.1
      </div>
      <div className="flex items-center gap-4 text-xs text-muted-foreground">
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
