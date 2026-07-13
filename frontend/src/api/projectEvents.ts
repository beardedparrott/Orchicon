// Project event streaming hooks (docs/10 §4 Realtime Model).
//
// useStreamProjectEvents wraps the generic useStream hook to subscribe
// to the StreamProjectEvents server-stream RPC. It provides a live feed
// of project lifecycle events (created, updated, archived, paused) that
// the UI renders in real-time.

import { useMemo } from "react";
import type { PartialMessage } from "@bufbuild/protobuf";

import { projectClient } from "@/api/clients";
import { useStream } from "@/api/useStream";
import type { ProjectEvent } from "@/api/gen/orchicon/api/v1/project_pb";
import type { StreamProjectEventsRequest } from "@/api/gen/orchicon/api/v1/project_service_pb";
import type { StreamProjectEventsResponse } from "@/api/gen/orchicon/api/v1/project_service_pb";

export interface UseStreamProjectEventsOpts {
  projectId?: string;
  enabled?: boolean;
  onEvent?: (event: ProjectEvent) => void;
}

export function useStreamProjectEvents(
  opts: UseStreamProjectEventsOpts,
) {
  const { projectId, enabled = true, onEvent } = opts;

  const request = useMemo<PartialMessage<StreamProjectEventsRequest>>(
    () => ({
      projectId: projectId ?? undefined,
    }),
    [projectId],
  );

  return useStream({
    name: "project-events",
    stream: (req) => projectClient.streamProjectEvents(req),
    request,
    getEventId: (resp: StreamProjectEventsResponse) => {
      const evt = resp.event;
      if (!evt) return "";
      // Use event_id if present; fall back to eventType+sequence for dedup.
      return evt.eventId || `${evt.eventType}-${resp.sequence}`;
    },
    getSequence: (resp: StreamProjectEventsResponse) => resp.sequence,
    filter: (resp: StreamProjectEventsResponse) => {
      if (!resp.event) return false;
      if (projectId && resp.event.projectId && resp.event.projectId !== projectId) {
        return false;
      }
      return true;
    },
    onEvent: (resp: StreamProjectEventsResponse) => {
      if (resp.event) {
        onEvent?.(resp.event);
      }
    },
    enabled,
  });
}
