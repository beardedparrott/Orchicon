// Pure helpers for the worker card display — split out for testability
// per the show-active-model-published-status-on-worker-cards ADR.
// Formatting matches ModelPicker (provider/id verbatim, e.g. anthropic/claude-sonnet-4).

import { WorkerVersionStatus } from "@/api/gen/orchicon/api/v1/worker_pb";

export function formatModelRef(ref: string): string {
  const trimmed = (ref ?? "").trim();
  if (trimmed === "") return "\u2014";
  return trimmed;
}

export function versionStatusLabel(status: WorkerVersionStatus): string {
  switch (status) {
    case WorkerVersionStatus.DRAFT:
      return "draft";
    case WorkerVersionStatus.PUBLISHED:
      return "published";
    case WorkerVersionStatus.DEPRECATED:
      return "deprecated";
    default:
      return "unknown";
  }
}

/**
 * Tailwind tone for the version publish badge. Visually distinct from
 * the Worker status badge (Worker uses lifecycle colors; version uses
 * outline vs solid distinction — here we reuse the same palette but
 * callers may add border styles).
 */
export function versionStatusTone(status: WorkerVersionStatus): string {
  switch (status) {
    case WorkerVersionStatus.DRAFT:
      return "bg-blue-100 text-blue-800 border border-blue-200";
    case WorkerVersionStatus.PUBLISHED:
      return "bg-green-100 text-green-800 border border-green-200";
    case WorkerVersionStatus.DEPRECATED:
      return "bg-amber-100 text-amber-800 border border-amber-200";
    default:
      return "bg-muted text-muted-foreground";
  }
}
