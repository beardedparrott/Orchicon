// Auth session: in-memory access token + refresh-on-401 + local/OIDC login.
//
// Per docs/10_Frontend_Architecture.md §7: the access token lives in
// memory (never localStorage); the refresh token lives in an HttpOnly
// cookie set by the backend. Token refresh is transparent; session
// expiry surfaces a re-auth prompt, not an error.
//
// The token holder is a module-level mutable variable so the Connect
// transport interceptor (clients.ts) can read it without a React
// context dependency (the transport is created at module load).
import { create } from "zustand";

const ACCESS_TOKEN_KEY = "orchicon_access_token";

// In-memory access token. Set on login/refresh; cleared on logout.
let accessToken = "";

// True once the user explicitly signs out in this page session. The router
// guard must never silently refresh a signed-out user back in via the
// still-valid HttpOnly refresh cookie — the server clears that cookie on
// logout too, but this flag covers the in-page case where the logout request
// failed or is still in flight. Any token acquisition resets it.
let signedOut = false;

// Load a token stashed by the OIDC callback route (URL fragment) so it
// survives the redirect into the SPA. The callback writes to
// sessionStorage then redirects to /, where this loads it into memory.
export function loadStashedToken(): boolean {
  const stashed = sessionStorage.getItem(ACCESS_TOKEN_KEY);
  if (stashed) {
    setAccessToken(stashed);
    sessionStorage.removeItem(ACCESS_TOKEN_KEY);
    return true;
  }
  return false;
}

export function getAccessToken(): string {
  return accessToken;
}

export function setAccessToken(t: string): void {
  signedOut = false;
  accessToken = t;
}

export function clearAccessToken(): void {
  accessToken = "";
}

// SessionInfo is the resolved identity context for the UI.
export type SessionInfo = {
  authenticated: boolean;
  identity_id?: string;
  tenant_id?: string;
  is_admin?: boolean;
  expires_at?: number;
  // The local credential's username (local-mode sessions only; carried on
  // the session response so it survives a full page load). The
  // change-password gate passes it to SetLocalCredential, which requires
  // the username to upsert.
  username?: string;
  // True when the signed-in local credential is flagged for a forced
  // password change (the bootstrap admin seeded with the built-in default).
  // The SPA renders a full-screen change-password gate while it is true;
  // the token is still issued so the gate can call the change RPC.
  force_password_change?: boolean;
};

// RefreshResult is a discriminated union for the refresh outcome.
// `ok: true` means the session is alive with a fresh token.
// `ok: false, reason: "no-session"` means the refresh token is absent,
// invalid, or expired — the session is genuinely over and re-login is
// correct. `ok: false, reason: "transient"` means a network error,
// 5xx, or server hiccup occurred — the session may still be valid and
// the in-memory token is NOT cleared (it can retry).
export type RefreshResult =
  | { ok: true; session: SessionInfo }
  | { ok: false; reason: "no-session" }
  | { ok: false; reason: "transient" };

// localLogin authenticates a local account against the embedded IdP with a
// username + password. The server verifies the stored argon2id/bcrypt hash,
// mints the token pair, sets the HttpOnly refresh cookie, and — when `next`
// is the OP login-bridge path the browser came from — completes the pending
// authorize request. The returned server-constructed `next` (if any) is the
// same-origin path to full-page-load so the OIDC flow finishes.
export async function localLogin(
  username: string,
  password: string,
  next?: string,
): Promise<{ session: SessionInfo; next?: string }> {
  const res = await fetch("/auth/local-login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ username, password, next: next ?? "" }),
  });
  if (!res.ok) {
    throw new Error(res.status === 401 ? "Invalid username or password" : `local-login failed: ${res.status}`);
  }
  const body = await res.json();
  setAccessToken(body.access_token);
  return {
    session: {
      authenticated: true,
      identity_id: body.identity_id,
      tenant_id: body.tenant_id,
      is_admin: body.is_admin,
      expires_at: Date.now() + body.expires_in * 1000,
      // The username just entered (the gate's SetLocalCredential call needs
      // it before any reload); the server re-sends it on /auth/session.
      username,
      force_password_change: body.force_password_change === true,
    },
    next: body.next ?? undefined,
  };
}

// signup creates a self-service local account over the embedded IdP and
// starts a session in one step. The server atomically provisions the
// identity + argon2id-hashed credential, mints the token pair, sets the
// HttpOnly refresh cookie, and — when `next` is the OP login-bridge path
// the browser came from — completes the pending authorize request (same
// contract as localLogin). A username that is already taken is rejected
// with 409.
export async function signup(
  username: string,
  password: string,
  next?: string,
): Promise<{ session: SessionInfo; next?: string }> {
  const res = await fetch("/auth/signup", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ username, password, next: next ?? "" }),
  });
  if (!res.ok) {
    throw new Error(
      res.status === 409
        ? "An account with this username already exists"
        : `signup failed: ${res.status}`,
    );
  }
  const body = await res.json();
  setAccessToken(body.access_token);
  return {
    session: {
      authenticated: true,
      identity_id: body.identity_id,
      tenant_id: body.tenant_id,
      is_admin: body.is_admin,
      expires_at: Date.now() + body.expires_in * 1000,
    },
    next: body.next ?? undefined,
  };
}

// oidcLogin returns the IdP authorize URL (the browser navigates there).
export function oidcLoginURL(): string {
  return "/auth/oidc/login";
}

// refreshAccessToken exchanges the HttpOnly refresh cookie for a new
// access token. Returns a discriminated RefreshResult so the caller
// can distinguish "session is over" (no-session) from "transient failure"
// (the in-memory token is NOT cleared on transient).
export async function refreshAccessToken(): Promise<RefreshResult> {
  try {
    const res = await fetch("/auth/refresh", {
      method: "POST",
      credentials: "include",
    });
    if (!res.ok) {
      // 401/403/404 = the refresh token is absent, invalid, or expired.
      // The session is genuinely over.
      if (res.status >= 400 && res.status < 500) {
        clearAccessToken();
        return { ok: false, reason: "no-session" };
      }
      // 5xx = server error. The session may still be valid; don't
      // clear the in-memory token.
      return { ok: false, reason: "transient" };
    }
    const body = await res.json();
    setAccessToken(body.access_token);
    return {
      ok: true,
      session: {
        authenticated: true,
        identity_id: body.identity_id,
        tenant_id: body.tenant_id,
        is_admin: body.is_admin,
        expires_at: Date.now() + body.expires_in * 1000,
      },
    };
  } catch {
    // Network error / parse failure: don't clear the in-memory token.
    return { ok: false, reason: "transient" };
  }
}

// fetchSession queries the backend for the current resolved identity.
export async function fetchSession(): Promise<SessionInfo> {
  const res = await fetch("/auth/session", {
    headers: accessToken ? { Authorization: `Bearer ${accessToken}` } : {},
    credentials: "include",
  });
  if (!res.ok) {
    return { authenticated: false };
  }
  const body = await res.json();
  if (!body.authenticated) {
    clearAccessToken();
  }
  return body as SessionInfo;
}

// Module-level bootstrap promise so concurrent callers (the router guard
// beforeLoad and the AuthProvider effect) share one session resolution
// instead of racing two fetches on a full page load.
let sessionPromise: Promise<SessionInfo> | null = null;

// ensureSession resolves the session exactly once per bootstrap. It is the
// single session-bootstrap path shared by the router auth guard (which runs
// before React mounts) and AuthProvider (after mount).
//
// A fresh page load has no in-memory access token (memory is not
// persistent), so the HttpOnly refresh cookie is exchanged for a new access
// token — a live session survives a reload without forcing a re-login. A
// genuinely logged-out visitor (no cookie) resolves unauthenticated and the
// guard redirects to /login.
//
// Transient refresh failures are retried (up to 3 times with backoff)
// before concluding unauthenticated — a transient DB hiccup or network
// blip no longer bounces an authenticated user to /login.
export function ensureSession(): Promise<SessionInfo> {
  if (!sessionPromise) {
    sessionPromise = (async (): Promise<SessionInfo> => {
      try {
        // The OIDC callback route stashes a token in sessionStorage on its
        // way in; load it into memory before deciding how to resolve.
        loadStashedToken();
        if (signedOut) {
          // Explicit sign-out earlier in this page session: never silently
          // re-authenticate via the refresh cookie (the server cleared it,
          // but don't depend on that request having landed).
          const unauth: SessionInfo = { authenticated: false };
          useSessionStore.getState().setSession(unauth);
          useSessionStore.getState().setLoading(false);
          return unauth;
        }
        if (!getAccessToken()) {
          // First load: try refreshing with retry for transient failures.
          const result = await refreshWithRetry();
          if (result.ok) {
            useSessionStore.getState().setSession(result.session);
            useSessionStore.getState().setLoading(false);
            return result.session;
          }
          // no-session or exhausted retries: unauthenticated.
          const unauth: SessionInfo = { authenticated: false };
          useSessionStore.getState().setSession(unauth);
          useSessionStore.getState().setLoading(false);
          return unauth;
        }
        // We already have a token (from OIDC callback or prior call):
        // verify it against the server.
        const s = await fetchSession();
        useSessionStore.getState().setSession(s);
        useSessionStore.getState().setLoading(false);
        return s;
      } catch {
        // Network failure / unexpected error: treat as unauthenticated so
        // the guard redirects to /login (never an error boundary).
        const unauth: SessionInfo = { authenticated: false };
        useSessionStore.getState().setSession(unauth);
        useSessionStore.getState().setLoading(false);
        return unauth;
      }
    })().finally(() => {
      sessionPromise = null;
    });
  }
  return sessionPromise;
}

// refreshWithRetry attempts refreshAccessToken with exponential backoff
// for transient failures. On a no-session result it returns immediately
// (no retry). Up to 3 attempts with ~1s, ~2s delays.
async function refreshWithRetry(): Promise<RefreshResult> {
  const delays = [0, 1000, 2000];
  for (let attempt = 0; attempt < delays.length; attempt++) {
    const result = await refreshAccessToken();
    if (result.ok) return result;
    if (result.reason === "no-session") return result; // definitive: no retry
    // Transient: wait and retry (if more attempts remain).
    if (attempt < delays.length - 1) {
      await new Promise((r) => setTimeout(r, delays[attempt + 1]));
    }
  }
  // All retries exhausted: treat as transient (caller gets no-session
  // since the in-memory token is still stale and will be stale).
  return { ok: false, reason: "no-session" };
}

// proactiveRefresh fires a background refresh when the access token
// is within 60 seconds of expiry and the document is visible. This
// prevents idle users from hitting a hard access-TTL boundary.
// Returns a RefreshResult; transient failures schedule one retry after
// ~5 seconds (bounded, no timer storm).
let proactiveRefreshScheduled = false;
let proactiveRefreshTimer: ReturnType<typeof setTimeout> | null = null;

export function scheduleProactiveRefresh(expiresAt: number): void {
  // Clear any existing timer.
  if (proactiveRefreshTimer) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
  proactiveRefreshScheduled = true;

  const now = Date.now();
  const remaining = expiresAt - now;
  if (remaining <= 0) {
    // Already expired: fire immediately.
    fireProactiveRefresh();
    return;
  }
  // Fire when < 60s of life remains.
  const fireIn = Math.max(0, remaining - 60_000);
  proactiveRefreshTimer = setTimeout(fireProactiveRefresh, fireIn);
}

// fireProactiveRefresh performs the actual refresh. On transient failure
// it schedules one retry after ~5s.
async function fireProactiveRefresh(): Promise<void> {
  proactiveRefreshScheduled = false;
  proactiveRefreshTimer = null;
  const result = await refreshAccessToken();
  if (result.ok) {
    // Update expires_at in the session store.
    useSessionStore.getState().setSession(result.session);
    return;
  }
  if (result.reason === "transient") {
    // Schedule one retry after 5s.
    proactiveRefreshTimer = setTimeout(async () => {
      const retry = await refreshAccessToken();
      if (retry.ok) {
        useSessionStore.getState().setSession(retry.session);
      }
    }, 5000);
  }
  // no-session: don't retry — the session is genuinely over.
}

// setupProactiveRefreshHook wires the proactive refresh to document
// visibility and focus events. When the tab becomes visible or focused,
// if the access token is expired or within 60s of expiry, refresh
// immediately. This catches idle users who return to a stale tab.
export function setupProactiveRefreshHook(): void {
  const handleVisibility = (): void => {
    if (document.visibilityState !== "visible") return;
    const expiresAt = useSessionStore.getState().session.expires_at;
    if (!expiresAt) return;
    const now = Date.now();
    const remaining = expiresAt - now;
    if (remaining <= 0 || remaining < 60_000) {
      fireProactiveRefresh();
    }
  };
  document.addEventListener("visibilitychange", handleVisibility);
  window.addEventListener("focus", handleVisibility);
}

// logout ends the session. The in-memory token is cleared and the signedOut
// flag prevents the router guard from silently re-authenticating via the
// refresh cookie for the rest of this page session. A best-effort
// POST /auth/logout clears the HttpOnly refresh cookie server-side too, so a
// reload stays signed out as well (the request failing — offline, etc. — is
// not fatal; the in-page flag still holds).
export function logout(): void {
  signedOut = true;
  clearAccessToken();
  // Cancel any pending proactive refresh timer.
  if (proactiveRefreshTimer) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
  void fetch("/auth/logout", { method: "POST", credentials: "include" }).catch(
    () => {
      /* best-effort: the in-page flag still prevents re-authentication */
    },
  );
}

// AuthConfig carries the plane's auth capability flags (GET /auth/config,
// public). The login page renders exactly the sign-in affordances the
// running plane supports; the values are server-driven capability mirrors,
// never client-side policy (AGENTS.md invariant #1).
export type AuthConfig = {
  mode: string;
  embedded_op: boolean;
  external_oidc: boolean;
  // signup advertises self-service account creation over the embedded IdP
  // (true exactly when the plane's embedded OP is enabled). The SPA shows
  // the "Create an account" affordance only when it is advertised.
  signup: boolean;
};

// fetchAuthConfig reads the plane's auth capability flags for the
// pre-login login page. Public endpoint — no credentials required.
export async function fetchAuthConfig(): Promise<AuthConfig> {
  const res = await fetch("/auth/config", { credentials: "include" });
  if (!res.ok) {
    throw new Error(`auth/config failed: ${res.status}`);
  }
  return (await res.json()) as AuthConfig;
}

// useSessionStore is a thin Zustand store for UI-only session state
// (docs/10 §6: UI-only state lives in Zustand). The server state (the
// resolved identity) is fetched via fetchSession and cached here.
type SessionState = {
  session: SessionInfo;
  loading: boolean;
  setSession: (s: SessionInfo) => void;
  setLoading: (b: boolean) => void;
};

export const useSessionStore = create<SessionState>((set) => ({
  session: { authenticated: false },
  loading: false,
  setSession: (s) => set({ session: s }),
  setLoading: (b) => set({ loading: b }),
}));
