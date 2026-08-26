import { Outlet, createRootRoute, useRouterState, Link } from "@tanstack/react-router";
import { PUBLIC_PATHS, requireAuth } from "@/auth/route-guard";
import { useSessionStore } from "@/auth/session";
import { AppShell } from "@/components/app-shell";
import { ForcePasswordGate } from "@/components/force-password-gate";
export const Route = createRootRoute({
  beforeLoad: requireAuth,
  component: RootComponent,
  notFoundComponent: NotFound,
});
function RootComponent() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const session = useSessionStore((s) => s.session);
  if (session.authenticated && session.force_password_change) {
    return <ForcePasswordGate />;
  }
  if (PUBLIC_PATHS.has(path)) {
    return <Outlet />;
  }
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
function NotFound() {
  return (
    <div className="flex flex-col items-center justify-center py-16 space-y-4 text-center">
      <h1 className="text-2xl font-semibold tracking-tight">Page not found</h1>
      <p className="text-sm text-muted-foreground max-w-md">
        The page you requested does not exist. It may have been moved or you may have followed an outdated link.
      </p>
      <div className="flex gap-2">
        <Link to="/dashboard" className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90">
          Go to Dashboard
        </Link>
        <Link to="/ask-orchicon" search={{ conversationId: null } as never} className="rounded-md border px-4 py-2 text-sm font-medium hover:bg-accent">
          Ask Orchicon
        </Link>
      </div>
    </div>
  );
}
