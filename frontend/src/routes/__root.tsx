import { Outlet, createRootRoute } from "@tanstack/react-router";

import { requireAuth } from "@/auth/route-guard";
import { useSessionStore } from "@/auth/session";
import { AppShell } from "@/components/app-shell";
import { ForcePasswordGate } from "@/components/force-password-gate";

// Root route — owns the application layout shell (docs/10 §5).
// Child routes render into the shell's <Outlet/>. The beforeLoad auth
// guard runs before any route (including children) renders, redirecting
// unauthenticated visitors to /login (docs/10 §7).
export const Route = createRootRoute({
  beforeLoad: requireAuth,
  component: RootComponent,
});

function RootComponent() {
  const session = useSessionStore((s) => s.session);
  if (session.authenticated && session.force_password_change) {
    // The signed-in credential is flagged for a forced password change:
    // the gate renders in place of the app content (ADR-6) — no app route
    // is reachable until the change completes.
    return <ForcePasswordGate />;
  }
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
