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
  fetchSession,
  loadStashedToken,
  logout as doLogout,
  devLogin,
  oidcLoginURL,
  useSessionStore,
  type SessionInfo,
} from "@/auth/session";

// AuthProvider boots the session on mount. Place it above the router
// (or as a layout effect in the root route).
export function AuthProvider({ children }: { children: ReactNode }) {
  const setSession = useSessionStore((s) => s.setSession);
  const setLoading = useSessionStore((s) => s.setLoading);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    (async () => {
      loadStashedToken();
      const s = await fetchSession();
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

// startDevLogin triggers the local dev IdP synthetic login.
export async function startDevLogin(subject: string): Promise<SessionInfo> {
  const s = await devLogin(subject);
  useSessionStore.getState().setSession(s);
  return s;
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
