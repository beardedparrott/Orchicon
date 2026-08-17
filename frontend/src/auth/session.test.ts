import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  ensureSession,
  getAccessToken,
  localLogin,
  logout,
  setAccessToken,
  signup,
  useSessionStore,
  type SessionInfo,
} from "@/auth/session";

// sessionStorage shim (node test env). loadStashedToken() reads the OIDC
// callback stash from it.
const storage = new Map<string, string>();
Object.defineProperty(globalThis, "sessionStorage", {
  configurable: true,
  value: {
    get length() {
      return storage.size;
    },
    getItem: (k: string) => storage.get(k) ?? null,
    removeItem: (k: string) => {
      storage.delete(k);
    },
    setItem: (k: string, v: string) => {
      storage.set(k, v);
    },
    clear: () => storage.clear(),
  },
});

function sessionBody(): SessionInfo & { access_token: string; expires_in: number } {
  return {
    access_token: "token-after-refresh",
    expires_in: 3600,
    authenticated: true,
    identity_id: "usr_test",
    tenant_id: "tnt_test",
    is_admin: true,
    expires_at: Date.now() + 3600 * 1000,
  };
}

function okResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("ensureSession", () => {
  beforeEach(() => {
    storage.clear();
    logout();
    // logout() also arms the signed-out flag (the sign-out regression guard);
    // setAccessToken("") resets it so each test starts as a fresh page load —
    // no in-memory token and not signed out.
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    storage.clear();
    logout();
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  it("exchanges the HttpOnly refresh cookie for a session on a fresh load (no in-memory token)", async () => {
    let requestedUrl = "";
    const fetchMock = vi.fn(async (url: string) => {
      requestedUrl = url;
      return okResponse(sessionBody());
    });
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();

    expect(s.authenticated).toBe(true);
    expect(s.identity_id).toBe("usr_test");
    // The refresh endpoint was used, not /auth/session (no bearer to send).
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(requestedUrl).toBe("/auth/refresh");
    // The resolved session is published to the store for the UI.
    expect(useSessionStore.getState().session.authenticated).toBe(true);
  });

  it("resolves unauthenticated when the refresh cookie is absent or expired", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 401 }));
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();

    expect(s.authenticated).toBe(false);
    expect(useSessionStore.getState().session.authenticated).toBe(false);
  });

  it("dedups concurrent callers onto a single bootstrap fetch", async () => {
    const fetchMock = vi.fn(async () => okResponse(sessionBody()));
    vi.stubGlobal("fetch", fetchMock);

    const [a, b] = await Promise.all([ensureSession(), ensureSession()]);

    expect(a.authenticated).toBe(true);
    expect(b.authenticated).toBe(true);
    // Two callers share one promise → one /auth/refresh round-trip.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("resolves unauthenticated (never throws) when the bootstrap fetch fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );

    const s = await ensureSession();
    expect(s.authenticated).toBe(false);
    expect(useSessionStore.getState().session.authenticated).toBe(false);
  });

  it("resolves via /auth/session when a stashed token was loaded into memory", async () => {
    // Simulate a page load that followed the OIDC callback stash.
    storage.set("orchicon_access_token", "stashed-token");
    const fetchMock = vi.fn(async (url: string) =>
      url === "/auth/session"
        ? okResponse({
            authenticated: true,
            identity_id: "usr_oidc",
            tenant_id: "tnt_test",
            is_admin: false,
          })
        : new Response(null, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();

    expect(s.authenticated).toBe(true);
    expect(s.identity_id).toBe("usr_oidc");
    // With an in-memory token the identity comes from /auth/session (the
    // refresh cookie is not consulted).
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/auth/session");
    // The stash is consumed (removed from sessionStorage).
    expect(globalThis.sessionStorage.getItem("orchicon_access_token")).toBeNull();
  });

  it("short-paths to /auth/session when an access token is already in memory", async () => {
    setAccessToken("existing-token");
    const fetchMock = vi.fn(async (url: string) =>
      url === "/auth/session"
        ? okResponse({
            authenticated: true,
            identity_id: "usr_mem",
            tenant_id: "tnt_test",
            is_admin: false,
          })
        : new Response(null, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();
    expect(s.authenticated).toBe(true);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/auth/session");
  });

  it("never re-authenticates a signed-out user via the still-valid refresh cookie", async () => {
    // The user signed out; the HttpOnly refresh cookie may still be valid.
    logout();
    const fetchMock = vi.fn(async () => okResponse(sessionBody()));
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();

    expect(s.authenticated).toBe(false);
    expect(useSessionStore.getState().session.authenticated).toBe(false);
    // No /auth/refresh round-trip: the signed-out flag short-circuits the
    // bootstrap, so the guard redirects to /login (AC1 holds for sign-out).
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("logout() asks the server to clear the HttpOnly refresh cookie", async () => {
    let calledUrl = "";
    let calledInit: RequestInit | undefined;
    const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      calledUrl = url;
      calledInit = init;
      return new Response(null, { status: 204 });
    });
    vi.stubGlobal("fetch", fetchMock);

    logout();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(calledUrl).toBe("/auth/logout");
    expect(calledInit?.method).toBe("POST");
    expect(calledInit?.credentials).toBe("include");
  });

  it("a fresh login resets the signed-out flag (refresh-on-boot works again)", async () => {
    logout();
    setAccessToken("token-after-re-login");
    const fetchMock = vi.fn(async (url: string) =>
      url === "/auth/session"
        ? okResponse({
            authenticated: true,
            identity_id: "usr_relogin",
            tenant_id: "tnt_test",
            is_admin: false,
          })
        : new Response(null, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const s = await ensureSession();
    expect(s.authenticated).toBe(true);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/auth/session");
  });
});

describe("signup", () => {
  beforeEach(() => {
    storage.clear();
    logout();
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    storage.clear();
    logout();
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  it("creates an account and starts a session (token in memory, next passed through)", async () => {
    let calledUrl = "";
    let calledBody = "";
    const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      calledUrl = url;
      calledBody = String(init?.body);
      return okResponse({
        access_token: "signup-token",
        token_type: "Bearer",
        expires_in: 3600,
        identity_id: "usr_signup",
        tenant_id: "tnt_dev",
        is_admin: false,
        next: "/authorize/callback?id=abc",
      });
    });
    vi.stubGlobal("fetch", fetchMock);

    const out = await signup("newuser", "password-123", "/auth/op/login?id=abc");

    expect(calledUrl).toBe("/auth/signup");
    expect(calledBody).toContain('"username":"newuser"');
    expect(calledBody).toContain('"password":"password-123"');
    expect(calledBody).toContain('"next":"/auth/op/login?id=abc"');
    expect(out.session.authenticated).toBe(true);
    expect(out.session.identity_id).toBe("usr_signup");
    expect(out.session.is_admin).toBe(false);
    expect(out.next).toBe("/authorize/callback?id=abc");
    // The access token is held in memory for the transport interceptor.
    expect(getAccessToken()).toBe("signup-token");
  });

  it("maps a 409 to the duplicate-username message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response("an account with this username already exists", { status: 409 }),
      ),
    );

    await expect(signup("taken", "password-123")).rejects.toThrow(
      "An account with this username already exists",
    );
  });

  it("surfaces other failures generically", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("nope", { status: 500 })),
    );

    await expect(signup("newuser", "password-123")).rejects.toThrow(/signup failed/);
  });
});

describe("localLogin", () => {
  beforeEach(() => {
    storage.clear();
    logout();
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    storage.clear();
    logout();
    setAccessToken("");
    useSessionStore.setState({ session: { authenticated: false }, loading: false });
  });

  it("maps the server-sourced forced-change flag onto the session (flagged bootstrap admin)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        okResponse({
          access_token: "login-token",
          token_type: "Bearer",
          expires_in: 3600,
          identity_id: "usr_admin",
          tenant_id: "tnt_test",
          is_admin: true,
          force_password_change: true,
        }),
      ),
    );

    const out = await localLogin("admin", "admin");

    // The flag is the gate signal: true drives the full-screen
    // change-password gate in place of the app content.
    expect(out.session.force_password_change).toBe(true);
    // The entered username rides on the session so the gate's
    // SetLocalCredential call has it before any reload.
    expect(out.session.username).toBe("admin");
    expect(out.session.authenticated).toBe(true);
    expect(getAccessToken()).toBe("login-token");
  });

  it("leaves the flag unset for unflagged credentials", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        okResponse({
          access_token: "login-token",
          token_type: "Bearer",
          expires_in: 3600,
          identity_id: "usr_pinned",
          tenant_id: "tnt_test",
          is_admin: true,
          // force_password_change omitted (omitempty) → unflagged.
        }),
      ),
    );

    const out = await localLogin("admin", "pinned-password");

    expect(out.session.force_password_change).toBe(false);
    expect(out.session.username).toBe("admin");
  });
});
