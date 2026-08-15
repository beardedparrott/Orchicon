import { Outlet, createRootRoute } from "@tanstack/react-router";

import { requireAuth } from "@/auth/route-guard";
import { AppShell } from "@/components/app-shell";

// Root route — owns the application layout shell (docs/10 §5).
// Child routes render into the shell's <Outlet/>. The beforeLoad auth
// guard runs before any route (including children) renders, redirecting
// unauthenticated visitors to /login (docs/10 §7).
export const Route = createRootRoute({
  beforeLoad: requireAuth,
  component: RootComponent,
});

function RootComponent() {
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
