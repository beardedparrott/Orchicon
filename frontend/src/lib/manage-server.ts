// manage-server.ts — the pure half of the Manage Server page.
//
// The state machine lives here, away from the component, so the four connection
// states and the action wording are unit-testable in the node environment (this
// suite has no jsdom). The component renders; this DECIDES.
import type { ActionDTO, PlaneState, ResolutionDTO, StatusDTO } from "@/lib/desktop";

export type View = {
  /** One of the four states. `connecting` is client-only: an action or the first status probe is in flight. */
  state: PlaneState;
  /** What the screen says it FOUND. Never a generic "error". */
  detail: string;
  /**
   * Where focus belongs when this view takes over. `primary` ONLY on the transition
   * INTO offline — never on a poll tick, or the window would steal focus every 2s.
   */
  focusTarget: "primary" | null;
};

/**
 * nextView folds (previous view, latest status, in-flight action) into what the page
 * shows.
 *
 * `pending` wins: while a start/stop/restart is in flight the state is `connecting`,
 * which is a statement about the ACTION, not a claim of health. A stale `connected`
 * must never survive an action that is restarting the plane.
 */
export function nextView(prev: View | null, status: StatusDTO | null, pending: boolean): View {
  if (pending) {
    return { state: "connecting", detail: "an action is in flight", focusTarget: null };
  }
  if (!status) {
    return { state: "connecting", detail: "contacting the plane…", focusTarget: null };
  }
  const enteredOffline = status.State === "offline" && prev?.state !== "offline";
  return {
    state: status.State,
    detail: status.Detail,
    focusTarget: enteredOffline ? "primary" : null,
  };
}

/** needsInstance is AC7's "ask rather than guess": an unrecognised control port must prompt. */
export function needsInstance(resolution: ResolutionDTO | null): boolean {
  return resolution !== null && !resolution.Known;
}

/** pickerOffered: with more than one plane up, the operator chooses. */
export function pickerOffered(resolution: ResolutionDTO | null): boolean {
  return resolution !== null && resolution.Running.length > 1;
}

/**
 * describeAction is the confirmation the page shows after an action. It NAMES THE
 * INSTANCE (so "which instance did I just start" is answerable from the screen) and,
 * on failure, the LOG PATH — never a success over a plane that was not verified
 * healthy: `started` comes only from the Go side's OK, which is only true after
 * planecontrol's bounded /healthz confirmation.
 */
export function describeAction(dto: ActionDTO): string {
  const verb = dto.Action === "start" ? "Start" : dto.Action === "stop" ? "Stop" : "Restart";
  const who = dto.Instance;
  switch (dto.Outcome) {
    case "started":
      return `${verb} ${who}: started and answering /healthz`;
    case "already-running":
      return `${verb} ${who}: already running — left as it is`;
    case "left-alone-unidentified":
      return `${verb} ${who}: left alone — the serving process could not be identified, so nothing was restarted`;
    case "stopped":
      return `${verb} ${who}: stopped`;
    case "already-stopped":
      return `${verb} ${who}: already stopped`;
    default:
      return `${verb} ${who}: failed — see ${dto.LogPath}`;
  }
}

/** stateLabel is the badge text: the same four words the state machine uses, spelled for a human. */
export function stateLabel(state: PlaneState): string {
  switch (state) {
    case "connecting":
      return "Connecting";
    case "connected":
      return "Connected";
    case "plane-error":
      return "Plane error";
    default:
      return "Offline";
  }
}

/** Instance choice as the page holds it: resolved, chosen, or still to be asked. */
export type InstanceChoice = {
  instance: string | null;
  /** true when the page must ASK: the port is unknown and the shell named no instance. */
  ask: boolean;
  /** the running planes the picker offers (more than one ⇒ the operator chooses). */
  running: StatusDTO[];
};

/**
 * resolveChoice combines the shell's instance (already a decision) with the Go side's
 * port resolution. The shell wins when it named one — it is the instance the app was
 * configured against — and an unrecognised port with no shell instance means ASK.
 */
export function resolveChoice(
  shellInstance: string | undefined,
  resolution: ResolutionDTO | null,
): InstanceChoice {
  if (shellInstance) {
    return { instance: shellInstance, ask: false, running: resolution?.Running ?? [] };
  }
  if (!resolution) {
    return { instance: null, ask: true, running: [] };
  }
  if (resolution.Known) {
    return { instance: resolution.Instance, ask: false, running: resolution.Running };
  }
  return { instance: null, ask: true, running: resolution.Running };
}

/**
 * currentStatus is the status the page may TRUST: only one that was read for the
 * instance currently selected. A DTO read for a different instance is stale by
 * definition, and showing it beside the newly selected instance's name and port would
 * claim a state nobody observed for that plane.
 */
export function currentStatus(
  selected: string | null,
  statusFor: string | null,
  status: StatusDTO | null,
): StatusDTO | null {
  if (!status || !selected || statusFor !== selected) return null;
  return status;
}

/**
 * statusReadFailure is the honest sentence for a status read that failed.
 *
 * The page must SAY a read failed rather than sit on "connecting" forever: a silent
 * spin over an unread status is the same dishonesty as a success that was never
 * verified, and it is exactly what "report a real failure rather than spinning" forbids.
 */
export function statusReadFailure(instance: string, err: unknown): string {
  const why = err instanceof Error ? err.message : String(err);
  return `${instance}: could not read the plane's status — ${why}`;
}
