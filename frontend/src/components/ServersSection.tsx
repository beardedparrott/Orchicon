/**
 * ServersSection.tsx — the SERVERS page's management surface (design of record, phase 1).
 *
 * WHAT THIS IS FOR. Before it, the desktop's Manage Server could start, stop and restart ONE instance's
 * plane — a control panel for a thing you could only create by typing a command. This adds the verbs a
 * person on their own machine actually needs: see every server, set one up, remove one, and watch a setup
 * finish.
 *
 * THE FOUR STATES ARE NOT POLISH, they are the page:
 *
 *   loading   "checking servers…" — never a blank frame, which reads as "there are none".
 *   empty     no servers yet, and ONE primary action. This is what a fresh install sees when setup has not
 *             started or has finished without leaving a plane.
 *   error     the failure, its cause and a retry. A failed read is SHOWN, never swallowed — a page that
 *             silently renders "no servers" over an unreachable shell is the worst of the four.
 *   denied    a browser (no shell to ask). Says why, rather than offering buttons that cannot work.
 *
 * THE DESTRUCTIVE ACTIONS ARE DELIBERATE. Remove asks which of two things you mean — keep the data, or
 * destroy it — and the destroying option names the secrets key, because deleting it does not delete secrets,
 * it makes them permanently undecryptable. A single "Remove" button that guessed would be the most
 * expensive convenience in the product.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  Loader2,
  Play,
  Plus,
  RefreshCw,
  RotateCcw,
  Square,
  Trash2,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  SETUP_DONE,
  SETUP_FAILED,
  SETUP_RUNNING,
  SETUP_STALE,
  isSetupActive,
  servers,
  setupLabel,
  type ServerInstance,
  type ServerResult,
  type SetupProgress,
} from "@/lib/servers";

/** POLL_MS is how often an in-flight setup is re-read. Fast enough to feel live, slow enough to be free. */
const POLL_MS = 1500;

type Phase = "loading" | "ready" | "error" | "denied";

export function ServersSection() {
  const api = useMemo(() => servers(), []);
  const [phase, setPhase] = useState<Phase>(api ? "loading" : "denied");
  const [items, setItems] = useState<ServerInstance[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string>("");
  const [notice, setNotice] = useState("");
  // The row whose destructive confirm is open, and which answer it is on. Kept as state rather than a
  // window.confirm so it is keyboard reachable and testable.
  const [confirmRemove, setConfirmRemove] = useState<string>("");
  const [adding, setAdding] = useState(false);
  const [newName, setNewName] = useState("");
  // A generation counter so a refresh that lands after an action cannot overwrite newer state with older.
  const gen = useRef(0);

  const refresh = useCallback(async () => {
    if (!api) return;
    const mine = ++gen.current;
    try {
      const got = await api.List();
      if (mine !== gen.current) return; // a newer refresh already answered
      setItems(got);
      setPhase("ready");
      setError("");
    } catch (e) {
      if (mine !== gen.current) return;
      // THE READ FAILURE IS THE CONTENT, not a blank page: say what happened and keep the retry reachable.
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }, [api]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // POLL WHILE ANY SETUP IS IN FLIGHT. Deliberately keyed on the SETUP STATES rather than on a timer that
  // always runs: an idle page must not poll a machine that is not doing anything.
  const anySetupActive = items.some((i) => isSetupActive(i.setup));
  useEffect(() => {
    if (!api || !anySetupActive) return;
    const t = window.setInterval(() => void refresh(), POLL_MS);
    return () => window.clearInterval(t);
  }, [api, anySetupActive, refresh]);

  const applyResult = useCallback(
    (r: ServerResult, verb: string) => {
      // The shell's outcome words are a CLOSED SET (internal/instancectl), so this maps them rather than
      // inventing prose per call site.
      const who = r.instance || "this machine";
      setNotice(
        r.ok
          ? `${verb} ${who}: ${r.outcome.replace(/-/g, " ")}`
          : `${verb} ${who} failed: ${r.message || r.outcome}`,
      );
      void refresh();
    },
    [refresh],
  );

  const run = useCallback(
    async (instance: string, verb: string, fn: () => Promise<ServerResult>) => {
      setBusy(instance || "*");
      setNotice("");
      try {
        applyResult(await fn(), verb);
      } catch (e) {
        setNotice(`${verb} failed: ${e instanceof Error ? e.message : String(e)}`);
        void refresh();
      } finally {
        setBusy("");
      }
    },
    [applyResult, refresh],
  );

  // ---------------------------------------------------------------- denied
  if (phase === "denied") {
    return (
      <p className="text-sm text-muted-foreground" data-testid="servers-denied">
        Server management is local to the machine running the desktop app. This
        page is open in a browser, so there is no host here to install on or
        remove from.
      </p>
    );
  }

  // ---------------------------------------------------------------- loading
  if (phase === "loading") {
    return (
      <p
        className="flex items-center gap-2 text-sm text-muted-foreground"
        data-testid="servers-loading"
      >
        <Loader2 aria-hidden="true" className="h-4 w-4 animate-spin" />
        Checking servers…
      </p>
    );
  }

  // ---------------------------------------------------------------- error
  if (phase === "error") {
    return (
      <div className="space-y-3" data-testid="servers-error">
        <p className="flex items-start gap-2 text-sm text-destructive">
          <AlertTriangle aria-hidden="true" className="mt-0.5 h-4 w-4 shrink-0" />
          <span className="min-w-0 [overflow-wrap:anywhere]">
            Could not read this machine&apos;s servers: {error}
          </span>
        </p>
        <Button variant="outline" size="sm" onClick={() => void refresh()}>
          <RefreshCw aria-hidden="true" className="mr-1 h-4 w-4" />
          Try again
        </Button>
      </div>
    );
  }

  // ---------------------------------------------------------------- empty
  if (items.length === 0 && !adding) {
    return (
      <div className="space-y-3" data-testid="servers-empty">
        <p className="text-sm text-muted-foreground">
          No servers yet. Setting one up creates everything it needs — a
          database, an event bus and the telemetry backends — on this machine.
        </p>
        <Button size="sm" onClick={() => setAdding(true)} data-testid="servers-add-first">
          <Plus aria-hidden="true" className="mr-1 h-4 w-4" />
          Set up Orchicon
        </Button>
        {notice && <p className="text-xs text-muted-foreground">{notice}</p>}
        {addingForm()}
      </div>
    );
  }

  // ---------------------------------------------------------------- ready
  return (
    <div className="space-y-3">
      {addingForm()}
      <ul className="space-y-2">
        {items.map((s) => (
          <ServerRow
            key={s.name}
            server={s}
            busy={busy === s.name}
            confirming={confirmRemove === s.name}
            onConfirm={() => setConfirmRemove(s.name)}
            onCancel={() => setConfirmRemove("")}
            onStart={() => void run(s.name, "start", () => api!.Start(s.name))}
            onStop={() => void run(s.name, "stop", () => api!.Stop(s.name))}
            onRestart={() => void run(s.name, "restart", () => api!.Restart(s.name))}
            onInstall={() => void run(s.name, "install", () => api!.Install(s.name))}
            onRemove={(purge) => {
              setConfirmRemove("");
              void run(s.name, "uninstall", () =>
                api!.Uninstall(s.name, { purge, confirmPurge: purge }),
              );
            }}
          />
        ))}
      </ul>
      {!adding && (
        <Button variant="outline" size="sm" onClick={() => setAdding(true)}>
          <Plus aria-hidden="true" className="mr-1 h-4 w-4" />
          Add another server
        </Button>
      )}
      {notice && (
        <p className="text-xs text-muted-foreground" data-testid="servers-notice">
          {notice}
        </p>
      )}
    </div>
  );

  function addingForm() {
    if (!adding) return null;
    return (
      <form
        className="flex flex-wrap items-end gap-2 rounded-md border p-3"
        data-testid="servers-add-form"
        onSubmit={(e) => {
          e.preventDefault();
          const name = newName.trim();
          if (!name) return;
          setAdding(false);
          setNewName("");
          void run(name, "install", () => api!.Install(name));
        }}
      >
        <label className="flex min-w-0 flex-col gap-1 text-xs">
          <span className="font-medium text-muted-foreground">Name</span>
          <Input
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            placeholder="prod"
            className="h-8 w-40"
            autoFocus
          />
        </label>
        <Button type="submit" size="sm" disabled={!newName.trim()}>
          Install
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => {
            setAdding(false);
            setNewName("");
          }}
        >
          Cancel
        </Button>
        <p className="w-full text-[11px] text-muted-foreground">
          Each server gets its own ports and its own copy of the service
          binaries. Leave the name as it is to re-apply an existing server
          without touching its data.
        </p>
      </form>
    );
  }
}

/** ServerRow is one server: what it is doing, and the verbs that change that. */
function ServerRow({
  server,
  busy,
  confirming,
  onConfirm,
  onCancel,
  onStart,
  onStop,
  onRestart,
  onInstall,
  onRemove,
}: {
  server: ServerInstance;
  busy: boolean;
  confirming: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  onStart: () => void;
  onStop: () => void;
  onRestart: () => void;
  onInstall: () => void;
  onRemove: (purge: boolean) => void;
}) {
  const setup = server.setup;
  const active = isSetupActive(setup);
  const running = server.plane.State === "connected" || server.plane.Healthy;
  // Setup IN FLIGHT owns the row: start/stop/restart while a database is being initialised would fight it.
  const verbsDisabled = busy || active;

  return (
    <li className="rounded-md border p-3" data-testid={`server-${server.name}`}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{server.name}</span>
        <StateBadge server={server} />
        {server.controlPort > 0 && (
          <span className="font-mono text-[11px] text-muted-foreground">
            :{server.controlPort}
          </span>
        )}
        {busy && (
          <Loader2
            aria-hidden="true"
            className="h-3.5 w-3.5 animate-spin text-muted-foreground"
          />
        )}
        <div className="ml-auto flex flex-wrap items-center gap-1">
          {!running && !active && (
            <Button variant="outline" size="sm" onClick={onStart} disabled={verbsDisabled}>
              <Play aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
              Start
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={onRestart}
            disabled={verbsDisabled}
            title="Stop and start this server"
          >
            <RotateCcw aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
            Restart
          </Button>
          {running && (
            <Button variant="outline" size="sm" onClick={onStop} disabled={verbsDisabled}>
              <Square aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
              Stop
            </Button>
          )}
          {!active && (
            <Button variant="outline" size="sm" onClick={onInstall} disabled={verbsDisabled}>
              Re-apply
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            onClick={onConfirm}
            disabled={busy}
            title="Remove this server"
          >
            <Trash2 aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
            Remove
          </Button>
        </div>
      </div>

      {setup && <SetupLine setup={setup} />}

      {confirming && (
        <RemoveConfirm name={server.name} onCancel={onCancel} onRemove={onRemove} />
      )}
    </li>
  );
}

/** StateBadge says what the PLANE is doing, in the words the rest of the app uses. */
function StateBadge({ server }: { server: ServerInstance }) {
  const setup = server.setup;
  if (isSetupActive(setup)) {
    return (
      <span className="rounded-full bg-sky-100 px-2 py-0.5 text-[10px] font-medium text-sky-800 dark:bg-sky-900/30 dark:text-sky-300">
        setting up
      </span>
    );
  }
  const healthy = server.plane.Healthy;
  const state = server.plane.State;
  const cls = healthy
    ? "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-300"
    : state === "plane-error"
      ? "bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-300"
      : "bg-muted text-muted-foreground";
  return (
    <span className={`rounded-full px-2 py-0.5 text-[10px] font-medium ${cls}`}>
      {healthy ? "running" : state === "plane-error" ? "error" : "stopped"}
    </span>
  );
}

/** SetupLine renders a setup record: progress while it works, and the two ways it can end badly. */
function SetupLine({ setup }: { setup: SetupProgress }) {
  if (setup.state === SETUP_RUNNING) {
    return (
      <div className="mt-2" data-testid="server-setup-progress">
        <div className="flex items-center justify-between text-[11px] text-muted-foreground">
          <span className="truncate">{setupLabel(setup)}</span>
          {setup.percent > 0 && <span className="ml-2 shrink-0">{setup.percent}%</span>}
        </div>
        {/* An INDETERMINATE bar when the step does not report a percentage: install's phases are not all
            measurable, and a bar frozen at 0% reads as stuck. */}
        <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-muted">
          <div
            className={`h-full rounded-full bg-sky-500 transition-[width] ${
              setup.percent > 0 ? "" : "animate-pulse"
            }`}
            style={{ width: `${setup.percent > 0 ? setup.percent : 100}%` }}
          />
        </div>
      </div>
    );
  }
  if (setup.state === SETUP_FAILED || setup.state === SETUP_STALE) {
    return (
      <p
        className="mt-2 flex items-start gap-2 text-[11px] text-destructive"
        data-testid="server-setup-failed"
      >
        <AlertTriangle aria-hidden="true" className="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <span className="min-w-0 [overflow-wrap:anywhere]">{setupLabel(setup)}</span>
      </p>
    );
  }
  if (setup.state === SETUP_DONE) {
    return (
      <p className="mt-2 text-[11px] text-muted-foreground" data-testid="server-setup-done">
        {setupLabel(setup)}
      </p>
    );
  }
  return null;
}

/**
 * RemoveConfirm is the two-answer question Remove asks.
 *
 * IT IS TWO BUTTONS, NOT ONE BUTTON AND A CHECKBOX, because the two outcomes are not degrees of the same
 * thing: one keeps a database you cannot recreate, the other destroys it and the key that decrypts your
 * secrets. Making them a checkbox next to a "Remove" label is how a mis-click becomes unrecoverable.
 */
function RemoveConfirm({
  name,
  onCancel,
  onRemove,
}: {
  name: string;
  onCancel: () => void;
  onRemove: (purge: boolean) => void;
}) {
  return (
    <div
      className="mt-3 space-y-2 rounded-md border border-destructive/40 bg-destructive/5 p-3"
      data-testid="server-remove-confirm"
    >
      <p className="text-xs font-medium">Remove the server “{name}”. What about its data?</p>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" onClick={() => onRemove(false)}>
          Keep the data
        </Button>
        <Button variant="destructive" size="sm" onClick={() => onRemove(true)}>
          Destroy the data
        </Button>
        <Button variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
      <p className="text-[11px] text-muted-foreground">
        Keeping the data removes the software and leaves the database, your
        attachments and the key that decrypts your secrets — reinstalling this
        server reuses them. Destroying it deletes the database and that key, so
        the secrets can never be decrypted again. There is no undo.
      </p>
    </div>
  );
}
