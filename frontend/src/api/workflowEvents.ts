// Workflow event streaming hooks (docs/10 §4 Realtime Model, §4.1).
//
// useStreamWorkflowEvents wraps the generic useStream hook to subscribe
// to the StreamWorkflowEvents server-stream RPC. The editor run view
// overlays live step transitions on the canvas (docs/10 §5.1).

import { useMemo } from "react";
import type { PartialMessage } from "@bufbuild/protobuf";

import { workflowClient } from "@/api/clients";
import { useStream } from "@/api/useStream";
import type { WorkflowEvent } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { StreamWorkflowEventsRequest } from "@/api/gen/orchicon/api/v1/workflow_service_pb";
import type { StreamWorkflowEventsResponse } from "@/api/gen/orchicon/api/v1/workflow_service_pb";

export interface UseStreamWorkflowEventsOpts {
  workflowId?: string;
  workflowRunId?: string;
  enabled?: boolean;
  onEvent?: (event: WorkflowEvent) => void;
}

export function useStreamWorkflowEvents(opts: UseStreamWorkflowEventsOpts) {
  const { workflowId, workflowRunId, enabled = true, onEvent } = opts;

  const request = useMemo<PartialMessage<StreamWorkflowEventsRequest>>(
    () => ({
      workflowId: workflowId ?? "",
      workflowRunId: workflowRunId ?? "",
    }),
    [workflowId, workflowRunId],
  );

  return useStream({
    name: "workflow-events",
    stream: (req) => workflowClient.streamWorkflowEvents(req),
    request,
    getEventId: (resp: StreamWorkflowEventsResponse) => {
      const evt = resp.event;
      if (!evt) return "";
      return `${evt.eventType}-${resp.sequence}`;
    },
    getSequence: (resp: StreamWorkflowEventsResponse) => resp.sequence,
    filter: (resp: StreamWorkflowEventsResponse) => {
      if (!resp.event) return false;
      if (workflowRunId && resp.event.workflowRunId && resp.event.workflowRunId !== workflowRunId) {
        return false;
      }
      if (workflowId && resp.event.workflowId && resp.event.workflowId !== workflowId) {
        return false;
      }
      return true;
    },
    onEvent: (resp: StreamWorkflowEventsResponse) => {
      if (resp.event) onEvent?.(resp.event);
    },
    enabled,
  });
}
