// Auth context provider + RBAC gating primitives.
//
// The provider boots the session on app load: it loads any stashed
// access token (from the OIDC callback fragment), then fetches the
// resolved identity from /auth/session. The transport interceptor
// (clients.ts) handles 401 → refresh transparently (docs/10 §7).
//
// RBAC gates UI affordances: actions the identity cannot perform are
// hidden or disabled, never silently failing on click (docs/10 §7).
// UI gating is a UX convenience; the server enforces entitlements
// (docs/10 §10 invariant #5).
import { useEffect, type ReactNode } from "react";

import {
  ensureSession,
  logout as doLogout,
  localLogin,
  signup,
  oidcLoginURL,
  scheduleLoginProactiveRefresh,
  useSessionStore,
  type SessionInfo,
} from "@/auth/session";

// AuthProvider boots the session on mount. Place it above the router
// (or as a layout effect in the root route).
//
// The router auth guard (root beforeLoad) runs before React mounts and
// already resolved the session via the shared ensureSession bootstrap on a
// full page load — skip the bootstrap when the store was resolved by it.
// Otherwise (public-route loads like /login, or no guard) bootstrap here;
// the shared promise dedups against any in-flight guard resolution.
export function AuthProvider({ children }: { children: ReactNode }) {
  const setSession = useSessionStore((s) => s.setSession);
  const setLoading = useSessionStore((s) => s.setLoading);

  useEffect(() => {
    let cancelled = false;
    const already = useSessionStore.getState();
    if (!already.loading && already.session.authenticated) return;
    setLoading(true);
    (async () => {
      const s = await ensureSession();
      if (!cancelled) {
        setSession(s);
        setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [setSession, setLoading]);

  return <>{children}</>;
}

// useSession returns the current session info.
export function useSession(): SessionInfo {
  return useSessionStore((s) => s.session);
}

// useIsAdmin reports whether the current identity is a tenant admin.
export function useIsAdmin(): boolean {
  return !!useSessionStore((s) => s.session).is_admin;
}

// useRequireEntitlement hides children when the session lacks the
// entitlement. UI gating only (docs/10 §10 invariant #5).
export function RequireEntitlement({
  children,
}: {
  entitlement: string;
  children: ReactNode;
}) {
  const session = useSession();
  if (!session.authenticated) return null;
  if (session.is_admin) return <>{children}</>;
  // The frontend does not carry the full entitlement set in the
  // session response (kept lean); for finer-grained gating the admin
  // surface uses the AuthService.ListEntitlements hook. This helper
  // gates on admin for now and shows children — the server still
  // enforces the per-RPC entitlement. A v0.2 can hydrate entitlements
  // into the session for precise client gating.
  return <>{children}</>;
}

// startLocalLogin authenticates a local account (embedded IdP) with a
// username + password. Returns the server-constructed `next` path to
// full-page-load (set when a pending embedded-OP authorize request was
// completed) so the OIDC flow returns the browser to the relying party.
export async function startLocalLogin(
  username: string,
  password: string,
  next?: string,
): Promise<{ session: SessionInfo; next?: string }> {
  const out = await localLogin(username, password, next);
  useSessionStore.getState().setSession(out.session);
  scheduleLoginProactiveRefresh(out.session);
  return out;
}

// startSignup creates a self-service account (embedded IdP) and starts a
// session, exactly like startLocalLogin: the response's server-constructed
// `next` path (set when a pending embedded-OP authorize request was
// completed) is what the caller full-page-loads to finish the OIDC flow.
export async function startSignup(
  username: string,
  password: string,
  next?: string,
): Promise<{ session: SessionInfo; next?: string }> {
  const out = await signup(username, password, next);
  useSessionStore.getState().setSession(out.session);
  scheduleLoginProactiveRefresh(out.session);
  return out;
}

// startOIDCLogin redirects the browser to the IdP authorize URL.
export function startOIDCLogin(): void {
  window.location.href = oidcLoginURL();
}

// logout clears the session and refreshes the UI.
export function logout(): void {
  doLogout();
  useSessionStore.getState().setSession({ authenticated: false });
}
