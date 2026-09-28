// Permissions tab — the GUI half of the persistent permission policy.
//
// The deny/accept list is DURABLE operator policy (a YAML file on the
// control plane), not a session grant: the deny list sits ABOVE a session
// grant in precedence, so an entry here cannot be overridden by granting
// consent in a conversation. That is why a deny row says so explicitly
// instead of offering a grant that the refusal would ignore.
//
// Precedence (the whole chain, for orientation):
//   never-allow binaries > deny > session grant > accept > project dir > ask
//
// Every mutation goes through the plane API, which reads and rewrites the
// file — this component never keeps its own copy, so an entry added here is
// immediately visible to the TUI and to a hand-edit of the file (and vice
// versa: a hand-edit appears here on the next refresh).
import { useState } from "react";

import {
  useGetPermissionPolicy,
  useAddPermissionPolicyEntry,
  useRemovePermissionPolicyEntry,
  denyEntries,
  acceptEntries,
} from "@/api/permissions";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Loader2, Plus, ShieldAlert, ShieldCheck, Trash2 } from "lucide-react";

export function PermissionsTab() {
  const { data, isLoading, error } = useGetPermissionPolicy();
  const add = useAddPermissionPolicyEntry();
  const remove = useRemovePermissionPolicyEntry();

  const [pattern, setPattern] = useState("");
  const [list, setList] = useState<"deny" | "accept">("deny");
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const entries = data?.entries ?? [];
  const deny = denyEntries(entries);
  const accept = acceptEntries(entries);

  async function handleAdd() {
    const p = pattern.trim();
    if (!p) {
      setMessage("An entry is required — an empty rule would match nothing.");
      return;
    }
    setMessage(null);
    try {
      await add.mutateAsync({ pattern: p, list });
      setPattern("");
      setMessage(`Added ${list} entry ${p}.`);
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    }
  }

  async function handleRemove(p: string, which: "deny" | "accept") {
    setMessage(null);
    setBusy(`${which}:${p}`);
    try {
      await remove.mutateAsync({ pattern: p, list: which });
      setMessage(`Removed ${which} entry ${p}.`);
    } catch (e) {
      setMessage(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldCheck className="h-4 w-4" />
            Permission policy
          </CardTitle>
          <CardDescription>
            Durable deny/accept policy, read on every gated decision — a change
            here takes effect on the next write or command, with no restart.
            {data?.path ? (
              <>
                {" "}
                The policy lives in a file the operator can edit by hand:{" "}
                <code className="rounded bg-muted px-1 py-0.5 text-xs">{data.path}</code>
              </>
            ) : null}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-end gap-2">
            <div className="grow">
              <label className="mb-1 block text-xs font-medium text-muted-foreground">
                Entry (doublestar glob; a leading ~ is your home)
              </label>
              <Input
                value={pattern}
                onChange={(e) => setPattern(e.target.value)}
                placeholder="~/.ssh/**"
                onKeyDown={(e) => {
                  if (e.key === "Enter") void handleAdd();
                }}
              />
            </div>
            <div>
              <label className="mb-1 block text-xs font-medium text-muted-foreground">
                List
              </label>
              <select
                value={list}
                onChange={(e) => setList(e.target.value === "accept" ? "accept" : "deny")}
                className="h-9 rounded-md border border-input bg-transparent px-2 text-sm"
              >
                <option value="deny">deny (refused)</option>
                <option value="accept">accept (never asks)</option>
              </select>
            </div>
            <Button onClick={() => void handleAdd()} disabled={add.isPending}>
              {add.isPending ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              <span className="ml-2">Add</span>
            </Button>
          </div>

          {message ? <p className="text-sm text-muted-foreground">{message}</p> : null}
          {error ? (
            <p className="text-sm text-destructive">
              Could not read the permission policy:{" "}
              {error instanceof Error ? error.message : String(error)}
            </p>
          ) : null}
          {isLoading ? (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" /> Loading…
            </p>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldAlert className="h-4 w-4" />
            Denied ({deny.length})
          </CardTitle>
          <CardDescription>
            Always refused. A session grant CANNOT override an entry here — the
            deny list outranks a grant, so the conversation's consent cannot
            open a path you excluded.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          {deny.length === 0 ? (
            <p className="text-sm text-muted-foreground">Nothing is denied.</p>
          ) : (
            deny.map((e) => (
              <div
                key={`deny:${e.pattern}`}
                className="flex items-center justify-between gap-3 rounded-md border border-white/10 px-3 py-2"
              >
                <div>
                  <code className="text-sm">{e.pattern}</code>
                  <p className="text-xs text-muted-foreground">
                    {e.overridable
                      ? "a session grant can override this entry"
                      : "a session grant cannot override this entry"}
                  </p>
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void handleRemove(e.pattern, "deny")}
                  disabled={busy === `deny:${e.pattern}`}
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            ))
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <ShieldCheck className="h-4 w-4" />
            Pre-accepted ({accept.length})
          </CardTitle>
          <CardDescription>
            Never prompts. These paths proceed silently, exactly as if the
            conversation had been granted them in advance.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          {accept.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              Nothing is pre-accepted — the conversation's own project directory
              is the only scope that proceeds without asking.
            </p>
          ) : (
            accept.map((e) => (
              <div
                key={`accept:${e.pattern}`}
                className="flex items-center justify-between gap-3 rounded-md border border-white/10 px-3 py-2"
              >
                <code className="text-sm">{e.pattern}</code>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => void handleRemove(e.pattern, "accept")}
                  disabled={busy === `accept:${e.pattern}`}
                >
                  <Trash2 className="h-4 w-4" />
                </Button>
              </div>
            ))
          )}
        </CardContent>
      </Card>
    </div>
  );
}
