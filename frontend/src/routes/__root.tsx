import { Outlet, createRootRoute } from "@tanstack/react-router";

import { AppShell } from "@/components/app-shell";

// Root route — owns the application layout shell (docs/10 §5).
// Child routes render into the shell's <Outlet/>.
export const Route = createRootRoute({
  component: RootComponent,
});

function RootComponent() {
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
