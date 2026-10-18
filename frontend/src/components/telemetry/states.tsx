// The five states of the native Telemetry page, in one place.
//
// Every panel renders its body through <TelemetryState> so no panel can
// invent its own wording — in particular a degraded backend must always be
// NAMED (AC 5), never folded into an empty chart:
//
//   loading        — the query is in flight
//   empty          — the store answered, but has nothing in this window
//   empty-filtered — the store answered and the identity/text filters
//                    excluded everything (a different fact from "no data")
//   degraded       — one or more stores are down; names which one and why,
//                    and explains that services are host processes
//   error          — the RPC itself failed (network/500)
//   denied         — the caller lacks telemetry:read

import { Code, ConnectError } from "@connectrpc/connect";
import type { ReactNode } from "react";

import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { formatCompact } from "@/lib/chart-geometry";

export type TelemetryStateKind =
  | "loading"
  | "empty"
  | "empty-filtered"
  | "degraded"
  | "error"
  | "denied"
  | "ready";

export interface DegradedBackend {
  /** Human name of the signal ("traces", "metrics", "logs"). */
  signal: string;
  /** Backend + cause reported by the server ("tempo is unreachable at …"). */
  reason: string;
}

/**
 * degradedBackends collects the downed backends from the per-signal query
 * results. The server reports `degraded` per signal with a `degradedReason`
 * naming the store, which is what lets the page distinguish "all three down"
 * from "one of three down" (the partial state).
 */
export function degradedBackends(
  queries: { signal: string; degraded?: boolean; degradedReason?: string }[],
): DegradedBackend[] {
  return queries
    .filter((q) => q.degraded === true)
    .map((q) => ({
      signal: q.signal,
      reason: q.degradedReason && q.degradedReason.trim() !== ""
        ? q.degradedReason
        : `${q.signal} backend is unreachable (no reason reported)`,
    }));
}

/** isPermissionDenied reports whether an RPC error is a missing permission. */
export function isPermissionDenied(err: unknown): boolean {
  if (!err) return false;
  if (err instanceof ConnectError) return err.code === Code.PermissionDenied;
  return false;
}

export interface TelemetryStateProps {
  kind: TelemetryStateKind;
  /** What is being loaded/shown ("traces", "reports"…). */
  signal: string;
  /** For kind="degraded". */
  degraded?: DegradedBackend[];
  /** For kind="empty"/"empty-filtered": a concrete next step. */
  hint?: string;
  /** For kind="error". */
  error?: unknown;
  onRetry?: () => void;
  children?: ReactNode;
  className?: string;
}

function Frame({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div
      className={cn(
        "flex min-h-[120px] flex-col items-center justify-center gap-1 rounded-md border border-dashed px-4 py-6 text-center text-sm text-muted-foreground",
        className,
      )}
    >
      {children}
    </div>
  );
}

/** TelemetryState renders exactly one of the page's states. */
export function TelemetryState({
  kind,
  signal,
  degraded,
  hint,
  error,
  onRetry,
  children,
  className,
}: TelemetryStateProps) {
  switch (kind) {
    case "ready":
      return <>{children}</>;
    case "loading":
      return (
        <Frame className={className}>
          <div
            role="status"
            aria-live="polite"
            className="flex items-center gap-2 text-xs uppercase tracking-wide"
          >
            <span
              aria-hidden="true"
              className="h-3 w-3 animate-pulse rounded-full bg-primary motion-reduce:animate-none"
            />
            Loading {signal}…
          </div>
        </Frame>
      );
    case "empty":
      return (
        <Frame className={className}>
          <p className="font-medium text-foreground">No {signal} in this window</p>
          <p className="text-xs">{hint ?? "Widen the time range, or run something to generate telemetry."}</p>
        </Frame>
      );
    case "empty-filtered":
      return (
        <Frame className={className}>
          <p className="font-medium text-foreground">No {signal} match the current filters</p>
          <p className="text-xs">
            The store answered — the project / execution / work-item / search filters excluded every
            result. {hint ?? "Clear a filter or widen the time range."}
          </p>
        </Frame>
      );
    case "degraded": {
      const down = degraded && degraded.length > 0 ? degraded : [{ signal, reason: `${signal} backend is unreachable` }];
      return (
        <div
          className={cn(
            "rounded-md border border-destructive/40 bg-destructive/5 px-4 py-3 text-sm",
            className,
          )}
          role="status"
        >
          <p className="font-medium text-foreground">
            {down.length === 1 ? `${down[0].signal} is unavailable` : `${down.length} telemetry backends are unavailable`}
          </p>
          <ul className="mt-1 list-disc space-y-1 pl-5 text-xs text-muted-foreground">
            {down.map((d) => (
              <li key={d.signal}>
                <span className="font-medium text-foreground">{d.signal}</span> — {d.reason}
              </li>
            ))}
          </ul>
          <p className="mt-2 text-xs text-muted-foreground">
            Telemetry services are host-resident processes started by <code>orchicon start</code> /{" "}
            <code>orchicon dev start</code> (Tempo, Loki, VictoriaMetrics + the OTel collector). They
            are not embedded in this page and do not start on their own — start the plane to bring
            them back. Data that lives in Postgres (the Reports view) keeps working meanwhile.
          </p>
        </div>
      );
    }
    case "denied":
      return (
        <Frame className={cn("border-destructive/40", className)}>
          <p className="font-medium text-foreground">Permission denied</p>
          <p className="text-xs">
            Reading telemetry requires the <code>telemetry:read</code> permission. Ask an
            administrator to grant it to your role.
          </p>
        </Frame>
      );
    case "error":
    default:
      return (
        <Frame className={cn("border-destructive/40", className)}>
          <p className="font-medium text-foreground">Could not load {signal}</p>
          <p className="text-xs">
            {error instanceof Error && error.message ? error.message : "The request failed."}
          </p>
          {onRetry ? (
            <Button size="sm" variant="outline" className="mt-2" onClick={onRetry}>
              Retry
            </Button>
          ) : null}
        </Frame>
      );
  }
}

/** StatCard is the summary tile used by Overview and Reports. */
export function StatCard({
  label,
  value,
  loading,
  hint,
}: {
  label: string;
  value: string;
  loading?: boolean;
  hint?: string;
}) {
  return (
    <Card className="glass-panel">
      <CardHeader className="pb-0">
        <CardDescription className="text-xs">{label}</CardDescription>
      </CardHeader>
      <CardContent className="pb-3 pt-1">
        <div className="text-xl font-semibold tabular-nums">{loading ? "…" : value}</div>
        {hint ? <div className="mt-0.5 text-[11px] text-muted-foreground">{hint}</div> : null}
      </CardContent>
    </Card>
  );
}

/** fmtInt renders a count (number|bigint) with thousands separators. */
export function fmtInt(n: number | bigint | undefined | null): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}

/** fmtCompactInt is fmtInt for stat values where space is tight. */
export function fmtCompactInt(n: number | bigint | undefined | null): string {
  if (n === undefined || n === null) return "0";
  return formatCompact(typeof n === "bigint" ? Number(n) : n);
}
