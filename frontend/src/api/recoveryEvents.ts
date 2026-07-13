// Recovery event streaming (docs/10 §4, docs/06 §11). The recovery
// timeline renders the full narrative with drill-down.

import { useMemo } from "react";
import type { PartialMessage } from "@bufbuild/protobuf";

import { recoveryClient } from "@/api/clients";
import { useStream } from "@/api/useStream";
import type { RecoveryEvent } from "@/api/gen/orchicon/api/v1/recovery_pb";
import type {
  StreamRecoveryEventsRequest,
  StreamRecoveryEventsResponse,
} from "@/api/gen/orchicon/api/v1/recovery_service_pb";

export interface UseStreamRecoveryEventsOpts {
  recoveryId?: string;
  projectId?: string;
  enabled?: boolean;
  onEvent?: (event: RecoveryEvent) => void;
}

export function useStreamRecoveryEvents(opts: UseStreamRecoveryEventsOpts) {
  const { recoveryId, projectId, enabled = true, onEvent } = opts;
  const request = useMemo<PartialMessage<StreamRecoveryEventsRequest>>(
    () => ({
      recoveryId: recoveryId ?? "",
      projectId: projectId ?? "",
    }),
    [recoveryId, projectId],
  );
  return useStream({
    name: "recovery-events",
    stream: (req) => recoveryClient.streamRecoveryEvents(req),
    request,
    getEventId: (resp: StreamRecoveryEventsResponse) => {
      const evt = resp.event;
      return evt ? `${evt.eventType}-${resp.sequence}` : "";
    },
    getSequence: (resp: StreamRecoveryEventsResponse) => resp.sequence,
    filter: (resp: StreamRecoveryEventsResponse) => {
      if (!resp.event) return false;
      if (recoveryId && resp.event.recoveryId && resp.event.recoveryId !== recoveryId) {
        return false;
      }
      if (projectId && resp.event.projectId && resp.event.projectId !== projectId) {
        return false;
      }
      return true;
    },
    onEvent: (resp: StreamRecoveryEventsResponse) => {
      if (resp.event) onEvent?.(resp.event);
    },
    enabled,
  });
}
