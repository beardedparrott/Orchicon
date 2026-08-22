import { useEffect, useState } from "react";
import { Link, createRoute, useNavigate, useSearch } from "@tanstack/react-router";

import { startSignup } from "@/auth/auth";
import { validateSignupForm } from "@/auth/signup-form";
import { fetchAuthConfig, type AuthConfig } from "@/auth/session";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Route as rootRoute } from "@/routes/__root";
import { router } from "@/router";

// Sign-up page — self-service account creation over the embedded IdP
// (docs/07 §6.1). The page is honest about the running plane: it fetches
// the public /auth/config capability flags and renders only when the plane
// advertises signup (signup availability == the embedded OP being enabled).
// Creating an account also starts a session: the server mints the token
// pair and sets the HttpOnly refresh cookie, so a successful sign-up lands
// the user in the app — no separate login step.
//
// Like the login page, the embedded OP's login bridge redirects
// unauthenticated users here with ?next=<same-origin path>. `next` is
// honored only when it is a same-origin path (no open redirect): after
// signing up the SPA performs a full page load so the browser returns
// through the bridge and completes the OP authorize flow.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/signup",
  component: SignupPage,
  validateSearch: (search: Record<string, unknown>): { next?: string } => ({
    next: typeof search.next === "string" ? search.next : undefined,
  }),
});

// safeNext returns the next path only when it is a same-origin absolute
// path: starts with "/", but is not "//host" or "///..." (which the
// browser treats as a scheme-relative URL).
function safeNext(raw: string | undefined): string | null {
  if (!raw || !raw.startsWith("/")) return null;
  if (raw.startsWith("//")) return null;
  if (raw.startsWith("/\\")) return null;
  return raw;
}

function SignupPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/signup" });
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [cfgError, setCfgError] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [formError, setFormError] = useState("");

  // Fetch the plane's auth capability flags once on mount.
  useEffect(() => {
    let cancelled = false;
    fetchAuthConfig()
      .then((c) => {
        if (!cancelled) setCfg(c);
      })
      .catch((e) => {
        if (!cancelled) setCfgError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const next = safeNext(search.next);

  // isSpaRoute reports whether the target pathname has a router route (the
  // discriminator between SPA destinations and server-only OP-bridge paths).
  function isSpaRoute(p: string): boolean {
    const pathname = p.split(/[?#]/)[0];
    return !!router.getMatchedRoutes(pathname).foundRoute;
  }

  function continueTo(target: string) {
    if (target && target !== "/") {
      if (isSpaRoute(target)) {
        // SPA route: navigate in-place so the in-memory access token set by
        // the signup response survives (a full page load would wipe it and
        // bounce the user straight back to /login).
        navigate({ to: target as never });
      } else {
        // Server-only path (embedded-OP bridge / authorize callback): the
        // router has no route for it, so the browser must load it directly
        // to complete the OIDC flow.
        window.location.assign(target);
      }
    } else {
      // Same-origin home after a plain (non-OP) signup: SPA-side navigate so
      // the in-memory access token set by the signup response survives.
      navigate({ to: "/" });
    }
  }

  async function handleSignup() {
    setError("");
    const clientErr = validateSignupForm({ username, password, confirm });
    if (clientErr) {
      setFormError(clientErr);
      return;
    }
    setFormError("");
    setBusy(true);
    try {
      const out = await startSignup(username.trim(), password, next ?? undefined);
      continueTo(out.next ?? next ?? "/");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Create your account</CardTitle>
          <CardDescription>
            Accounts are managed by Orchicon's embedded identity provider.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {cfgError && <p className="text-sm text-destructive">{cfgError}</p>}
          {cfg && !cfg.embedded_op && (
            <p className="text-sm text-muted-foreground">
              Self-service sign-up is not available on this plane.
            </p>
          )}
          <div className="space-y-2">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="you@example.com"
              autoComplete="username"
              autoCapitalize="none"
              autoCorrect="off"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">Password</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="At least 8 characters"
              autoComplete="new-password"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="confirm">Confirm password</Label>
            <Input
              id="confirm"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              placeholder="Repeat your password"
              autoComplete="new-password"
              onKeyDown={(e) => {
                if (e.key === "Enter") handleSignup();
              }}
            />
          </div>
          {formError && <p className="text-sm text-destructive">{formError}</p>}
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button
            className="w-full"
            onClick={handleSignup}
            disabled={busy || !cfg?.embedded_op}
          >
            {busy ? "Creating account…" : "Create account"}
          </Button>
          <div className="text-center text-sm">
            <span className="text-muted-foreground">Already have an account? </span>
            <Link
              to="/login"
              search={next ? { next } : undefined}
              className="text-primary hover:underline"
            >
              Sign in
            </Link>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
