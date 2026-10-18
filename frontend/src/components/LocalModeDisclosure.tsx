import { cn } from "@/lib/utils";

/**
 * The safety disclosure rendered wherever `local` execution mode is chosen or observed.
 *
 * LOCAL MODE IS NOT CONTAINED, AND THAT MUST BE SAID WHERE THE CHOICE IS MADE. In `runtime`
 * mode each execution runs in a short-lived, root-free container — a real isolation boundary.
 * In `local` mode the worker *is* a host process running as the operator, with the operator's
 * own filesystem access. The two layers that remain (opencode's permission deny rules and the
 * OS-level execution guard) are defense-in-depth, not containment: a worker that resolves the
 * real binary by absolute path, or that writes its own binary and runs that, gets past them.
 *
 * This component deliberately UNDER-SELLS rather than reassures. It names what is *not*
 * protected first, enumerates the surviving layers honestly as best-effort, and points at the
 * one real boundary. Do not soften it into a generic "sandboxed" banner: a permanent reassuring
 * banner trains operators to ignore the one moment it matters.
 *
 * The copy lives here once, so the three surfaces that render it (project create, project
 * edit, work-item form) cannot drift apart. Static text only — nothing focusable, no state, no
 * fetch — which is what makes it keyboard- and screen-reader-safe by construction.
 */
export function LocalModeDisclosure({ className }: { className?: string }) {
  return (
    <div
      role="note"
      aria-labelledby="local-mode-disclosure-title"
      data-testid="local-mode-disclosure"
      className={cn(
        "rounded-xl border border-amber-500/40 bg-amber-500/[0.06] p-3 text-xs",
        className,
      )}
    >
      <p
        id="local-mode-disclosure-title"
        className="font-semibold text-amber-700 dark:text-amber-300"
      >
        Local mode is not contained.
      </p>
      <div className="mt-1 space-y-1 text-muted-foreground">
        <p>
          Work runs as a <strong>host process with your own filesystem access</strong> — a worker
          can read and write anything you can.
        </p>
        <ul className="list-disc space-y-1 pl-4">
          <li>
            <strong className="text-foreground">Not protected:</strong> the opencode permission
            deny rules and the execution guard are{" "}
            <strong className="text-foreground">defense-in-depth, not containment</strong>. A
            determined worker escapes them — for example by invoking{" "}
            <code className="font-mono">/bin/rm</code> by absolute path, or by writing its own
            binary and running that.
          </li>
          <li>
            <strong className="text-foreground">Protected (best-effort):</strong> opencode
            permission rules (project-boundary deny on every path outside the project, plus the{" "}
            <code className="font-mono">/tmp/orchicon/**</code> scratch carve-out and the
            never-allow binary class), the OS execution guard that shims dangerous binaries ahead
            of the worker&apos;s <code className="font-mono">PATH</code>, and the worker prompt&apos;s
            Safety rules. All three are defense-in-depth.
          </li>
          <li>
            <strong className="text-foreground">If isolation matters, use runtime containers.</strong>{" "}
            Set <strong className="text-foreground">Execution mode → runtime</strong> for this
            project; each execution then runs in a short-lived, root-free container that cannot
            touch the host.
          </li>
        </ul>
      </div>
    </div>
  );
}
