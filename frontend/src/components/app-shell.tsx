import type { ReactNode } from "react";
import { Link, useRouterState } from "@tanstack/react-router";

import { cn } from "@/lib/utils";

// Application layout shell (docs/10_Frontend_Architecture.md §5).
//
// The shell is a thin client: it renders navigation and server-driven
// content. No business logic lives here (AGENTS.md invariant #1). Nav
// mirrors the API services; routes are added as slices land.

type NavItem = {
  label: string;
  to: string;
  disabled?: boolean;
};

const NAV: NavItem[] = [
  { label: "Dashboard", to: "/" },
  { label: "Projects", to: "/projects" },
  { label: "Work Items", to: "/work-items" },
  { label: "Workers", to: "/workers" },
  { label: "Workflows", to: "/workflows", disabled: true },
  { label: "Policies", to: "/policies", disabled: true },
  { label: "Recovery", to: "/recovery", disabled: true },
  { label: "Executions", to: "/executions" },
  { label: "Telemetry", to: "/telemetry", disabled: true },
  { label: "Adapters", to: "/adapters" },
  { label: "Admin", to: "/admin", disabled: true },
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
  return (
    <aside className="hidden w-60 shrink-0 border-r bg-card md:block">
      <div className="flex h-14 items-center gap-2 border-b px-5">
        <span className="text-lg font-semibold tracking-tight">Orchicon</span>
      </div>
      <nav className="space-y-1 p-3">
        {NAV.map((item) => {
          const active = path === item.to;
          if (item.disabled) {
            return (
              <span
                key={item.to}
                className="flex cursor-not-allowed items-center rounded-md px-3 py-2 text-sm text-muted-foreground/50"
                title="Coming soon"
              >
                {item.label}
              </span>
            );
          }
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
  return (
    <header className="flex h-14 items-center justify-between border-b px-6">
      <div className="text-sm text-muted-foreground">
        Orchicon control plane · v0.1
      </div>
      <div className="text-xs text-muted-foreground">
        {/* Auth flow arrives in a later phase (docs/10 §7). */}
        not signed in
      </div>
    </header>
  );
}
