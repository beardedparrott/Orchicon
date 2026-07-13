// Auth session: in-memory access token + refresh-on-401 + OIDC/dev login.
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

// Load a token stashed by the OIDC callback route (URL fragment) so it
// survives the redirect into the SPA. The callback writes to
// sessionStorage then redirects to /, where this loads it into memory.
export function loadStashedToken(): boolean {
  const stashed = sessionStorage.getItem(ACCESS_TOKEN_KEY);
  if (stashed) {
    accessToken = stashed;
    sessionStorage.removeItem(ACCESS_TOKEN_KEY);
    return true;
  }
  return false;
}

export function getAccessToken(): string {
  return accessToken;
}

export function setAccessToken(t: string): void {
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
};

// devLogin mints a synthetic token via the dev IdP (local mode only).
export async function devLogin(subject: string): Promise<SessionInfo> {
  const res = await fetch("/auth/dev-login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ subject }),
  });
  if (!res.ok) {
    throw new Error(`dev-login failed: ${res.status}`);
  }
  const body = await res.json();
  setAccessToken(body.access_token);
  return {
    authenticated: true,
    identity_id: body.identity_id,
    tenant_id: body.tenant_id,
    is_admin: body.is_admin,
    expires_at: Date.now() + body.expires_in * 1000,
  };
}

// oidcLogin returns the IdP authorize URL (the browser navigates there).
export function oidcLoginURL(): string {
  return "/auth/oidc/login";
}

// refreshAccessToken exchanges the HttpOnly refresh cookie for a new
// access token. Returns null if the refresh failed (expired/revoked).
export async function refreshAccessToken(): Promise<SessionInfo | null> {
  const res = await fetch("/auth/refresh", {
    method: "POST",
    credentials: "include",
  });
  if (!res.ok) {
    clearAccessToken();
    return null;
  }
  const body = await res.json();
  setAccessToken(body.access_token);
  return {
    authenticated: true,
    identity_id: body.identity_id,
    tenant_id: body.tenant_id,
    is_admin: body.is_admin,
    expires_at: Date.now() + body.expires_in * 1000,
  };
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

// logout clears the in-memory token. The refresh cookie is allowed to
// expire naturally (or the user clears cookies). A v0.2 could add a
// server-side revocation endpoint.
export function logout(): void {
  clearAccessToken();
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
