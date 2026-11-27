// AntigravityAdapterCard — the `antigravity` (agy) adapter's detail block on Settings → Adapters.
//
// WHY THIS CARD EXISTS. The generic adapter card answers "did this adapter register, at what version,
// on which endpoint". For `agy` that is not the question an operator has: agy is a VENDOR CLI that
// Orchicon launches, so the questions are "is it installed here", "how does it sign in", "what do its
// terms require me to know", and "is the API-key secret present". They were answerable only from the
// Settings → Defaults tab (and, in the TUI, only from the tenant-settings pane). This is the same
// information on the surface the adapter registry actually lives on.
//
// THREE RULES THIS FILE KEEPS:
//   1. The ToS text is the SERVER's (`TenantSettings.antigravityTosNotice`) — the same string the TUI
//      and the desktop client render. It is never re-typed here.
//   2. The auth method is READ-ONLY here, with a pointer to the one editable control (Settings →
//      Defaults). A second write path for the same setting is how two sources of truth appear.
//   3. The API-key secret is PRESENCE ONLY. The list API does not return values, and there is no code
//      path in this component that could show one.
import { useGetSettings } from "@/api/settings";
import { useSecretList } from "@/api/secrets";
import { ANTIGRAVITY_NOT_INSTALLED_MESSAGE } from "@/lib/adapter-errors";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

/** The tenant secret the `gemini-api-key` auth method reads (authmethod.APIKeySecretName). */
export const ANTIGRAVITY_API_KEY_SECRET = "GEMINI_API_KEY";

/** The four methods agy accepts, in the order the Settings select offers them (authmethod.Methods). */
export const ANTIGRAVITY_AUTH_METHODS = [
  { value: "oauth-personal", label: "Personal Google sign-in (oauth-personal)" },
  { value: "oauth-business", label: "Google Workspace (oauth-business)" },
  { value: "gemini-api-key", label: "Gemini API key (gemini-api-key)" },
  { value: "agent-platform", label: "Agent platform (agent-platform)" },
] as const;

/** methodCaveat is the one fact an operator needs about the SELECTED method. Mirrors the TUI's
 *  antigravityMethodSummary so the two clients describe one setting identically. */
export function methodCaveat(method: string): string {
  switch (method) {
    case "oauth-personal":
      return "host keyring — local runs only, cannot be used inside a runtime container";
    case "gemini-api-key":
      return `reads the tenant secret ${ANTIGRAVITY_API_KEY_SECRET}`;
    default:
      return "";
  }
}

interface AntigravityAdapterCardProps {
  /** The registered runtime adapter's version, when the plane has one registered. */
  version?: string;
  /** Whether the agy binary was found on this host. Absent = unknown (the plane reports no probe). */
  installed?: boolean;
}

export function AntigravityAdapterCard({ version, installed }: AntigravityAdapterCardProps) {
  const { data: settings } = useGetSettings();
  const { data: secrets } = useSecretList();

  // An UNSET method means the server's default (oauth-personal), the same normalization the TUI applies
  // (authmethod.Normalize) — an empty select row reads as "no method".
  const method = settings?.antigravityAuthMethod || "oauth-personal";
  const methodKnown = ANTIGRAVITY_AUTH_METHODS.some((m) => m.value === method);
  const hasAPIKey = (secrets ?? []).some((s) => s.name === ANTIGRAVITY_API_KEY_SECRET);
  const tosNotice = settings?.antigravityTosNotice ?? "";

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <span className="font-mono">antigravity</span>
          <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
            agy
          </span>
        </CardTitle>
        <CardDescription>
          Google&apos;s own <code>agy</code> CLI, driven over ACP. Orchicon runs agy; it never reads or
          stores your Google credentials.
          {version ? ` Registered: v${version}.` : ""}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3 text-xs">
        {/* THE BINARY. "Not installed" is the state that makes every other row moot, so it comes first
            and carries the install instruction rather than a generic "unavailable". */}
        <div>
          <span className="text-muted-foreground">Binary: </span>
          {installed === false ? (
            <span className="text-amber-700 dark:text-amber-400">
              {ANTIGRAVITY_NOT_INSTALLED_MESSAGE}
            </span>
          ) : (
            <span className="font-mono">agy</span>
          )}
        </div>

        {/* THE AUTH METHOD — read-only here, editable in Settings → Defaults (one write path). */}
        <div className="space-y-1">
          <div>
            <span className="text-muted-foreground">Auth method: </span>
            <span className="font-mono">{method}</span>
            {methodKnown && methodCaveat(method) ? (
              <span className="text-muted-foreground"> — {methodCaveat(method)}</span>
            ) : null}
          </div>
          <label className="grid gap-1">
            <span className="text-muted-foreground">
              Auth method (change in Settings → Defaults; both clients read this one value)
            </span>
            <select
              className="h-8 w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-xs"
              value={method}
              disabled
              aria-label="Antigravity auth method (read-only; edit in Settings → Defaults)"
            >
              {ANTIGRAVITY_AUTH_METHODS.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
          </label>
        </div>

        {/* THE ToS NOTICE — the server's exact text, on exactly the method whose terms it is about. */}
        {method === "oauth-personal" && tosNotice ? (
          <p className="rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-amber-900 dark:text-amber-200">
            {tosNotice}
          </p>
        ) : null}

        <div>
          <span className="text-muted-foreground">Telemetry: </span>
          {settings?.antigravityShareTelemetry ? "shared with Google" : "off (--disable-telemetry)"}
        </div>

        {/* THE SECRET — PRESENCE ONLY. There is no branch here that could read a value. */}
        <div>
          <span className="text-muted-foreground">API key secret: </span>
          {hasAPIKey ? (
            <span className="font-mono">{ANTIGRAVITY_API_KEY_SECRET} set</span>
          ) : (
            <span className="text-muted-foreground">
              not set — needed for the gemini-api-key method and for container runs
            </span>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
