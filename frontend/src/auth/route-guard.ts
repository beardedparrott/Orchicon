// Router-level auth guard (docs/10 §7).
//
// The root route's beforeLoad runs for every route in the tree (all routes
// are flat children of the root), so this single guard protects every app
// route at navigation time — before any protected component renders. The
// guard is deny-by-default: a route is public only when its pathname is in
// the explicit PUBLIC_PATHS allowlist. The server is the real enforcement
// backstop (every RPC 401s without a credential); this guard is the UX gate
// that keeps logged-out users from reaching the shell at all.
import { redirect } from "@tanstack/react-router";

import { ensureSession, useSessionStore } from "@/auth/session";

// Routes that render without authentication. /login is the sign-in page;
// /auth/callback is the OIDC fragment-token landing route (it arrives here
// with a token in the URL fragment before the session store resolves).
// Add future public routes (e.g. a marketing landing under the SPA) here.
export const PUBLIC_PATHS = new Set(["/login", "/auth/callback"]);

// requireAuth is the root beforeLoad guard. It fast-paths on an
// already-resolved session (SPA navigations are zero-cost) and only awaits
// the shared ensureSession bootstrap on a fresh page load / reload. An
// unauthenticated visitor is redirected to /login with the intended
// destination preserved in ?next= so a successful login returns there.
export async function requireAuth({ location }: { location: { pathname: string } }) {
  if (PUBLIC_PATHS.has(location.pathname)) return;
  if (useSessionStore.getState().session.authenticated) return;
  const s = await ensureSession();
  if (!s.authenticated) {
    throw redirect({ to: "/login", search: { next: location.pathname } });
  }
}
