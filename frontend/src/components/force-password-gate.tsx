import { useState } from "react";

import { useSetLocalCredential } from "@/api/auth";
import { useSessionStore } from "@/auth/session";
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

// ForcePasswordGate is the full-screen change-password gate rendered in
// place of the app content while the signed-in local credential is flagged
// for a forced password change (the bootstrap admin seeded with the
// built-in default admin/admin). It calls the admin-gated
// SetLocalCredential RPC (the bootstrap admin is a tenant admin, so RBAC
// passes); a successful set clears the flag server-side, and the store is
// updated immediately so the app content appears without a reload. The
// server re-resolves the flag on the next /auth/session, so the store
// update is a UX fast-path, not the enforcement (docs/10 §7).
export function ForcePasswordGate() {
  const session = useSessionStore((s) => s.session);
  const setSession = useSessionStore((s) => s.setSession);
  const setCredential = useSetLocalCredential();
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");

  const mismatch = confirm !== "" && password !== confirm;

  async function handleSubmit() {
    if (!session.identity_id || !session.username) {
      setError("Missing identity or username — sign in again.");
      return;
    }
    if (password === "") {
      setError("Enter a new password.");
      return;
    }
    if (password !== confirm) {
      setError("Passwords do not match.");
      return;
    }
    setError("");
    try {
      await setCredential.mutateAsync({
        identityId: session.identity_id,
        username: session.username,
        password,
      });
      setSession({ ...session, force_password_change: false });
    } catch (e) {
      setError(String(e));
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Change your password</CardTitle>
          <CardDescription>
            This plane was seeded with the default credential and the
            password must be changed before you can continue. The default
            password stops working as soon as the new one is set.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="new-password">New password</Label>
            <Input
              id="new-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              onKeyDown={(e) => {
                if (e.key === "Enter") handleSubmit();
              }}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="confirm-password">Confirm new password</Label>
            <Input
              id="confirm-password"
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              onKeyDown={(e) => {
                if (e.key === "Enter") handleSubmit();
              }}
            />
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          <Button
            className="w-full"
            onClick={handleSubmit}
            disabled={setCredential.isPending || mismatch}
          >
            {setCredential.isPending ? "Saving…" : "Save password"}
          </Button>
        </CardContent>
      </Card>
    </div>
  );
}
