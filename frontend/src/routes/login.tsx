import { useState } from "react";
import { createRoute, useNavigate, useSearch } from "@tanstack/react-router";

import { startDevLogin, startLocalLogin, startOIDCLogin } from "@/auth/auth";
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

// Login page (docs/10 §7). Local accounts of the embedded IdP sign in with
// a username + password; the dev IdP synthetic login is the local-mode dev
// fallback; OIDC SSO is the production path. The access token lands in
// memory; the refresh token in an HttpOnly cookie.
//
// The embedded OpenID Provider's login bridge redirects unauthenticated
// users here with ?next=<same-origin path>. `next` is honored only when
// it is a same-origin path (no open redirect): after authenticating the
// SPA performs a full page load so the browser returns through the bridge
// and completes the OP authorize flow.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
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

function LoginPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/login" });
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [subject, setSubject] = useState("dev@orchicon.local");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const next = safeNext(search.next);

  function continueTo(target: string) {
    if (target) {
      // Full page load: the OP bridge paths have no SPA route, so the
      // router cannot navigate there — the browser must hit them directly.
      window.location.assign(target);
    } else {
      navigate({ to: "/" });
    }
  }

  async function handleLocalLogin() {
    setBusy(true);
    setError("");
    try {
      // `next` (the URL the OP bridge bounced us from) is passed through so
      // the server can complete the pending authorize request; the response
      // carries the server-constructed path to continue the OIDC flow.
      const out = await startLocalLogin(username, password, next ?? undefined);
      continueTo(out.next ?? next ?? "/");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function handleDevLogin() {
    setBusy(true);
    setError("");
    try {
      await startDevLogin(subject);
      continueTo(next ?? "/");
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
          <CardTitle>Sign in to Orchicon</CardTitle>
          <CardDescription>
            Authenticate to access the control plane.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="you@example.com"
              autoComplete="username"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="password">Password</Label>
            <Input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              onKeyDown={(e) => {
                if (e.key === "Enter") handleLocalLogin();
              }}
            />
          </div>
          <Button className="w-full" onClick={handleLocalLogin} disabled={busy}>
            {busy ? "Signing in…" : "Sign in"}
          </Button>
          <div className="relative py-2">
            <div className="absolute inset-0 flex items-center">
              <span className="w-full border-t" />
            </div>
            <div className="relative flex justify-center text-xs uppercase">
              <span className="bg-card px-2 text-muted-foreground">or</span>
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="subject">Subject (dev IdP)</Label>
            <Input
              id="subject"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              placeholder="you@example.com"
            />
          </div>
          <Button variant="outline" className="w-full" onClick={handleDevLogin} disabled={busy}>
            {busy ? "Signing in…" : "Dev sign in"}
          </Button>
          <Button
            variant="outline"
            className="w-full"
            onClick={() => {
              setBusy(true);
              startOIDCLogin();
            }}
            disabled={busy}
          >
            Continue with SSO (OIDC)
          </Button>
          {error && (
            <p className="text-sm text-destructive">{error}</p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
