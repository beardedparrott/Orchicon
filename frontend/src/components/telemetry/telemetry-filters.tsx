// Telemetry filter bar — one bar drives every tab (overview / reports /
// traces / metrics / logs / credits).
//
// Patterned on components/work-items/work-items-filter-bar.tsx (the house
// filter-bar shape): native <select>/<label> controls so keyboard support
// comes for free, a Search-icon input, a flex-wrap row so it reflows on
// resize, and a polite live region describing the active window.

import { Search } from "lucide-react";
import { useEffect, useState } from "react";

import type { Project } from "@/api/gen/orchicon/api/v1/project_pb";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export type TimeRangePreset = "1h" | "6h" | "24h" | "7d" | "30d" | "custom";

export interface TelemetryFilterValue {
  range: TimeRangePreset;
  /** datetime-local values, used when range === "custom". */
  customStart: string;
  customEnd: string;
  projectId: string;
  executionId: string;
  /** A single work item id ("" = all). */
  workItemId: string;
  /** OTel severity text for logs ("" = all). */
  severity: string;
  search: string;
}

export const DEFAULT_TELEMETRY_FILTERS: TelemetryFilterValue = {
  range: "24h",
  customStart: "",
  customEnd: "",
  projectId: "",
  executionId: "",
  workItemId: "",
  severity: "",
  search: "",
};

const RANGE_MS: Record<Exclude<TimeRangePreset, "custom">, number> = {
  "1h": 60 * 60 * 1000,
  "6h": 6 * 60 * 60 * 1000,
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
  "30d": 30 * 24 * 60 * 60 * 1000,
};

export const RANGE_OPTIONS: { value: TimeRangePreset; label: string }[] = [
  { value: "1h", label: "Last hour" },
  { value: "6h", label: "Last 6 hours" },
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "custom", label: "Custom…" },
];

export const SEVERITY_OPTIONS = ["", "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"];

/** windowFor resolves the preset (or the custom inputs) into a concrete
 *  window. An unparseable custom value falls back to the 24 h preset so the
 *  query always has a real window. */
export function windowFor(
  value: TelemetryFilterValue,
  now: number = Date.now(),
): { start: Date; end: Date } {
  if (value.range === "custom") {
    const start = value.customStart ? new Date(value.customStart) : undefined;
    const end = value.customEnd ? new Date(value.customEnd) : undefined;
    if (start && end && !Number.isNaN(start.getTime()) && !Number.isNaN(end.getTime())) {
      return { start, end };
    }
    if (start && !Number.isNaN(start.getTime())) {
      return { start, end: new Date(now) };
    }
  }
  const span = RANGE_MS[value.range === "custom" ? "24h" : value.range];
  return { start: new Date(now - span), end: new Date(now) };
}

/** filterSignature is the query key component for the window. */
export function filterSignature(value: TelemetryFilterValue, now: number = Date.now()): string {
  const { start, end } = windowFor(value, now);
  return `${start.toISOString()}..${end.toISOString()}`;
}

export interface TelemetryFiltersProps {
  value: TelemetryFilterValue;
  onChange: (next: TelemetryFilterValue) => void;
  projects?: Project[];
  /** Disable the identity controls a signal cannot honour (metrics). */
  disableIdentity?: boolean;
  className?: string;
}

export function TelemetryFilters({
  value,
  onChange,
  projects,
  disableIdentity = false,
  className,
}: TelemetryFiltersProps) {
  const [draftSearch, setDraftSearch] = useState(value.search);

  // Debounce the free-text search (300 ms) so typing does not fire a query
  // per keystroke; keep the field in sync when the parent resets it.
  useEffect(() => {
    setDraftSearch(value.search);
  }, [value.search]);

  useEffect(() => {
    if (draftSearch === value.search) return;
    const id = window.setTimeout(() => onChange({ ...value, search: draftSearch }), 300);
    return () => window.clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draftSearch]);

  const selectClass =
    "h-9 rounded-xl glass-input px-3 text-sm focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50";

  return (
    <div
      role="group"
      aria-label="Telemetry filters"
      className={cn("flex flex-wrap items-center gap-2 rounded-xl glass-panel p-2", className)}
    >
      <select
        value={value.range}
        onChange={(e) => onChange({ ...value, range: e.target.value as TimeRangePreset })}
        aria-label="Time range"
        className={selectClass}
      >
        {RANGE_OPTIONS.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>

      {value.range === "custom" && (
        <>
          <label className="sr-only" htmlFor="telemetry-window-start">
            Window start
          </label>
          <Input
            id="telemetry-window-start"
            type="datetime-local"
            value={value.customStart}
            onChange={(e) => onChange({ ...value, customStart: e.target.value })}
            className="h-9 w-auto"
          />
          <label className="sr-only" htmlFor="telemetry-window-end">
            Window end
          </label>
          <Input
            id="telemetry-window-end"
            type="datetime-local"
            value={value.customEnd}
            onChange={(e) => onChange({ ...value, customEnd: e.target.value })}
            className="h-9 w-auto"
          />
        </>
      )}

      <select
        value={value.projectId}
        onChange={(e) => onChange({ ...value, projectId: e.target.value })}
        aria-label="Project"
        className={cn(selectClass, "max-w-[12rem]")}
      >
        <option value="">All projects</option>
        {(projects ?? []).map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
      </select>

      <Input
        aria-label="Execution id"
        placeholder="Execution id"
        value={value.executionId}
        onChange={(e) => onChange({ ...value, executionId: e.target.value })}
        disabled={disableIdentity}
        title={disableIdentity ? "Metrics are tenant + project scoped; executions are not a metric label" : undefined}
        className="h-9 w-40 font-mono text-xs"
      />

      <Input
        aria-label="Work item id"
        placeholder="Work item id"
        value={value.workItemId}
        onChange={(e) => onChange({ ...value, workItemId: e.target.value })}
        disabled={disableIdentity}
        title={
          disableIdentity
            ? "Metrics are tenant + project scoped; work items are not a metric label"
            : "Logs filter on task_id, traces on orchicon.work_item_id"
        }
        className="h-9 w-40 font-mono text-xs"
      />

      <select
        value={value.severity}
        onChange={(e) => onChange({ ...value, severity: e.target.value })}
        aria-label="Log severity"
        className={selectClass}
      >
        {SEVERITY_OPTIONS.map((s) => (
          <option key={s || "all"} value={s}>
            {s === "" ? "Any severity" : s}
          </option>
        ))}
      </select>

      <div className="relative min-w-[180px] flex-1">
        <Search
          aria-hidden="true"
          className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
        />
        <Input
          aria-label="Search log text"
          placeholder="Search log text…"
          value={draftSearch}
          onChange={(e) => setDraftSearch(e.target.value)}
          className="h-9 w-full pl-8"
        />
      </div>

      <span aria-live="polite" className="text-[11px] text-muted-foreground">
        {describeWindow(value)}
      </span>
    </div>
  );
}

function describeWindow(value: TelemetryFilterValue): string {
  const { start, end } = windowFor(value);
  const fmt = (d: Date) =>
    d.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  const scope: string[] = [];
  if (value.projectId) scope.push("project");
  if (value.executionId) scope.push("execution");
  if (value.workItemId) scope.push("work item");
  if (value.severity) scope.push(value.severity);
  if (value.search) scope.push("text");
  return `${fmt(start)} → ${fmt(end)}${scope.length ? ` · ${scope.join(", ")}` : ""}`;
}
