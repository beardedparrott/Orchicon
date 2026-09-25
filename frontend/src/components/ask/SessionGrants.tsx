import { useEffect, useState } from "react";
import { KeyRound } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  useListPermissionGrants,
  useRevokePermissionGrant,
} from "@/api/askOrchicon";
import { relativeGrantAge } from "@/lib/ask-consent";
import { useToast } from "@/components/ui/toast";

// SessionGrants — this conversation's ACTIVE session grants, with revoke.
//
// Deliberately a small disclosure rather than a panel: it is a short list that
// most of the time holds nothing, and it belongs beside the conversation it
// applies to, not in Settings. The plane's grant store is the source of truth
// (the revoke RPC returns the refreshed list), so this component holds no copy
// and a revoke is visible to the very next tool call.
//
// A grant is the "Allow for this session" answer for ONE directory. The
// persistent deny/accept list is a different surface with different semantics
// (Settings → Permissions): an entry there is not revocable from here because it
// is not a grant.

export interface SessionGrantsProps {
  conversationId: string;
  className?: string;
}

export function SessionGrants({ conversationId, className }: SessionGrantsProps) {
  const [open, setOpen] = useState(false);
  const toast = useToast();
  const { data: grants = [], isLoading } = useListPermissionGrants(conversationId, {
    // Poll while the panel is open so a grant given in another tab (or by a
    // decision just made on the card) appears; at rest the header shows the
    // count from whatever the last read returned.
    refetchInterval: open ? 5000 : false,
  });
  const revoke = useRevokePermissionGrant(conversationId);

  // Escape closes the panel — the same "Escape never silently dismisses
  // something the turn is waiting on" rule the cards follow; nothing here is
  // pending, so closing is all it means.
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open]);

  // Switching conversation closes it: the list belongs to one conversation.
  useEffect(() => {
    setOpen(false);
  }, [conversationId]);

  const count = grants.length;

  return (
    <div className={cn("relative shrink-0", className)}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-label="Session permission grants"
        data-testid="session-grants-trigger"
        className="flex h-11 items-center gap-1.5 rounded-xl border border-black/10 px-2.5 text-xs text-muted-foreground hover:text-foreground glass-panel dark:border-white/10"
      >
        <KeyRound aria-hidden="true" className="h-4 w-4" />
        Grants{count > 0 ? ` (${count})` : ""}
      </button>

      {open && (
        <div
          data-testid="session-grants-panel"
          className="absolute right-0 top-full z-40 mt-2 w-80 rounded-xl border border-border bg-popover p-3 text-sm shadow-lg"
        >
          <div className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            Session grants for this conversation
          </div>
          {count === 0 ? (
            <p className="text-xs text-muted-foreground" data-testid="session-grants-empty">
              {isLoading
                ? "Loading…"
                : "No session grants for this conversation."}
            </p>
          ) : (
            <ul className="flex flex-col gap-1.5">
              {grants.map((g) => (
                <li
                  key={g.directory}
                  data-testid="session-grant-row"
                  className="flex items-center justify-between gap-2 rounded-lg border border-border px-2 py-1.5"
                >
                  <span className="min-w-0">
                    <span className="block truncate font-mono text-xs" title={g.directory}>
                      {g.directory}
                    </span>
                    <span className="block text-[11px] text-muted-foreground">
                      {relativeGrantAge(Number(g.grantedAtUnix)) || "granted"}
                    </span>
                  </span>
                  <button
                    type="button"
                    data-testid="session-grant-revoke"
                    disabled={revoke.isPending}
                    onClick={() =>
                      revoke.mutate(g.directory, {
                        onSuccess: (res) => {
                          if (!res.removed) {
                            toast.error("That grant was already gone.", {
                              title: "Nothing to revoke",
                            });
                          }
                        },
                        onError: () =>
                          toast.error("Could not revoke the grant.", { title: "Error" }),
                      })
                    }
                    className="shrink-0 rounded-md border border-border px-2 py-0.5 text-xs hover:border-destructive hover:text-destructive disabled:opacity-50"
                  >
                    Revoke
                  </button>
                </li>
              ))}
            </ul>
          )}
          <p className="mt-2 text-[11px] text-muted-foreground">
            Revoking takes effect on the next tool call. A deny entry in
            Settings → Permissions always outranks a grant.
          </p>
        </div>
      )}
    </div>
  );
}

export default SessionGrants;
