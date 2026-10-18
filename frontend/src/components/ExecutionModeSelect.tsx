import { Label } from "@/components/ui/label";

/**
 * ExecutionModeSelect — the work-item execution-mode override.
 *
 * The choice is three-way because "inherit" is a REAL state, not the absence of
 * one: the work_items.execution_mode column is nullable and NULL means "use the
 * project's mode". The option text therefore SPELLS OUT the inherited value, so
 * a user always answers "where will this run?" before saving — the resolution
 * order is item -> project -> tenant default, implemented once on the server
 * (db.EffectiveExecutionMode).
 */
export type ExecutionModeChoice = "inherit" | "runtime" | "local";

/** The concrete modes, as the server stores them. */
export type ExecutionModeValue = "runtime" | "local";

/** Wire enum values (proto/orchicon/api/v1/project.proto ExecutionMode). */
export const EXECUTION_MODE_PROTO = {
  runtime: 1, // EXECUTION_MODE_RUNTIME
  local: 2, // EXECUTION_MODE_LOCAL
} as const;

export function executionModeToProto(v: ExecutionModeChoice): number | undefined {
  if (v === "inherit") return undefined; // UNSPECIFIED = inherit
  return EXECUTION_MODE_PROTO[v];
}

/**
 * executionModeValueOf reads a project's (or item's) mode from a proto message
 * that may arrive in either casing: protoc-gen-es emits camelCase, but some
 * serialized/mocked blobs carry the snake_case column name.
 */
export function executionModeValueOf(
  entity: { executionMode?: unknown; execution_mode?: unknown } | undefined | null,
): ExecutionModeValue | undefined {
  const raw = entity?.executionMode ?? entity?.execution_mode;
  if (raw === 2 || raw === "local" || raw === "LOCAL") return "local";
  if (raw === 1 || raw === "runtime" || raw === "RUNTIME") return "runtime";
  return undefined;
}

/** Maps a work item's stored override (may be absent = inherit) to the choice. */
export function executionModeChoiceOf(
  entity: { executionMode?: unknown; execution_mode?: unknown } | undefined | null,
): ExecutionModeChoice {
  return executionModeValueOf(entity) ?? "inherit";
}

/**
 * effectiveExecutionModeOf answers "where will this run?" from the item: the
 * server-resolved effective_execution_mode when present (single-item reads),
 * else the item override, else the project's mode, else the tenant default
 * (runtime). It never returns undefined, so the UI can always state an answer.
 */
export function effectiveExecutionModeOf(
  item: { effectiveExecutionMode?: unknown; effective_execution_mode?: unknown; executionMode?: unknown; execution_mode?: unknown } | undefined | null,
  projectMode: ExecutionModeValue | undefined,
): ExecutionModeValue {
  const fromServer = item?.effectiveExecutionMode ?? item?.effective_execution_mode;
  if (fromServer === 2 || fromServer === "local" || fromServer === "LOCAL") return "local";
  if (fromServer === 1 || fromServer === "runtime" || fromServer === "RUNTIME") return "runtime";
  return executionModeValueOf(item) ?? projectMode ?? "runtime";
}

export function ExecutionModeSelect({
  value,
  inherited,
  onChange,
  disabled,
  id = "execution-mode",
}: {
  value: ExecutionModeChoice;
  /** The project's mode — shown inside the inherit option so it is never a guess. */
  inherited: ExecutionModeValue;
  onChange: (v: ExecutionModeChoice) => void;
  disabled?: boolean;
  id?: string;
}) {
  const effective =
    value === "inherit" ? inherited : value;
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>Execution mode</Label>
      <select
        id={id}
        disabled={disabled}
        value={value}
        onChange={(e) => onChange(e.target.value as ExecutionModeChoice)}
        className="w-full rounded-xl glass-input px-3 py-1.5 text-sm"
      >
        <option value="inherit">Inherit from project ({inherited})</option>
        <option value="runtime">runtime — always-container</option>
        <option value="local">local — in-process (fenced)</option>
      </select>
      <p className="text-xs text-muted-foreground leading-relaxed">
        This item runs in <span className="font-medium">{effective}</span> mode.
        {value === "inherit"
          ? " Inherited from the project; changing the project changes this item."
          : " Overrides the project for this item only."}{" "}
        <span className="font-medium">runtime</span> provisions a container per run;{" "}
        <span className="font-medium">local</span> runs in-process with the honest prompt and a
        hard DSN fence.
      </p>
    </div>
  );
}
