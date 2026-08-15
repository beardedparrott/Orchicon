import { isRedirect } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { logout, setAccessToken, useSessionStore } from "@/auth/session";
import { PUBLIC_PATHS, requireAuth } from "@/auth/route-guard";

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

const ctx = (pathname: string) => ({ location: { pathname } });

describe("requireAuth", () => {
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

  it("throws a /login redirect with ?next= for a protected route when unauthenticated", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 401 })));

    let thrown: unknown;
    try {
      await requireAuth(ctx("/work-items"));
    } catch (e) {
      thrown = e;
    }
    expect(thrown).toBeDefined();
    expect(isRedirect(thrown as Response)).toBe(true);
    const opts = (thrown as Response & { options: { to: string; search: { next?: string } } }).options;
    expect(opts.to).toBe("/login");
    expect(opts.search.next).toBe("/work-items");
  });

  it("covers every public path in the allowlist without resolving a session", async () => {
    const fetchMock = vi.fn(async () => {
      throw new Error("session should not be resolved for a public path");
    });
    vi.stubGlobal("fetch", fetchMock);

    for (const path of PUBLIC_PATHS) {
      await expect(requireAuth(ctx(path))).resolves.toBeUndefined();
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("fast-paths when the store is already authenticated (no fetch, no redirect)", async () => {
    useSessionStore.setState({ session: { authenticated: true, identity_id: "usr_a" } });
    const fetchMock = vi.fn(async () => {
      throw new Error("bootstrap must not run when the session is resolved");
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(requireAuth(ctx("/work-items"))).resolves.toBeUndefined();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("allows a protected route when the bootstrap resolves authenticated (reload path)", async () => {
    // Live HttpOnly cookie: /auth/refresh succeeds → authenticated.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            access_token: "fresh-token",
            expires_in: 3600,
            authenticated: true,
            identity_id: "usr_reload",
            tenant_id: "tnt_test",
            is_admin: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(requireAuth(ctx("/work-items/01HXYZ"))).resolves.toBeUndefined();
    expect(useSessionStore.getState().session.authenticated).toBe(true);
  });

  it("passes a session loaded into memory (OIDC callback arrival) through", async () => {
    setAccessToken("callback-token");
    useSessionStore.setState({ session: { authenticated: false } });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(
          JSON.stringify({
            authenticated: true,
            identity_id: "usr_cb",
            tenant_id: "tnt_test",
            is_admin: false,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    await expect(requireAuth(ctx("/ask-orchicon"))).resolves.toBeUndefined();
    expect(useSessionStore.getState().session.authenticated).toBe(true);
  });
});
