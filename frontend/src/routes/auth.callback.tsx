import { useEffect } from "react";
import { createRoute, useNavigate } from "@tanstack/react-router";

import { setAccessToken } from "@/auth/session";
import { Route as rootRoute } from "@/routes/__root";

// OIDC callback route (docs/10 §7). The backend redirects to
// /#/auth/callback?access_token=...&expires_in=... after exchanging the
// IdP code + minting an Orchicon access token. The fragment keeps the
// token out of server logs. This route stashes the token into memory +
// session (for the reload) and redirects to the dashboard.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/auth/callback",
  component: AuthCallback,
});

function AuthCallback() {
  const navigate = useNavigate();
  useEffect(() => {
    // window.location.hash = "#/auth/callback?access_token=...&..."
    const hash = window.location.hash.replace(/^#\/auth\/callback\??/, "");
    const params = new URLSearchParams(hash);
    const token = params.get("access_token");
    if (token) {
      setAccessToken(token);
      // Stash so the session bootstrap on the next load picks it up.
      sessionStorage.setItem("orchicon_access_token", token);
    }
    navigate({ to: "/" });
  }, [navigate]);
  return (
    <div className="p-8 text-sm text-muted-foreground">
      Completing sign in…
    </div>
  );
}
