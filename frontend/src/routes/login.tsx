import { useState } from "react";
import { createRoute, useNavigate } from "@tanstack/react-router";

import { startDevLogin, startOIDCLogin } from "@/auth/auth";
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

// Login page (docs/10 §7). In local mode the dev IdP synthetic login is
// shown; OIDC SSO is the production path. The access token lands in
// memory; the refresh token in an HttpOnly cookie.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: LoginPage,
});

function LoginPage() {
  const navigate = useNavigate();
  const [subject, setSubject] = useState("dev@orchicon.local");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function handleDevLogin() {
    setBusy(true);
    setError("");
    try {
      await startDevLogin(subject);
      navigate({ to: "/" });
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
            Authenticate to access the control plane. In local mode the
            dev identity provider mints a short-lived token.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="subject">Subject (dev IdP)</Label>
            <Input
              id="subject"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              placeholder="you@example.com"
            />
          </div>
          <Button className="w-full" onClick={handleDevLogin} disabled={busy}>
            {busy ? "Signing in…" : "Dev sign in"}
          </Button>
          <div className="relative py-2">
            <div className="absolute inset-0 flex items-center">
              <span className="w-full border-t" />
            </div>
            <div className="relative flex justify-center text-xs uppercase">
              <span className="bg-card px-2 text-muted-foreground">or</span>
            </div>
          </div>
          <Button
            variant="outline"
            className="w-full"
            onClick={() => startOIDCLogin()}
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
