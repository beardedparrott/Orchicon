// Runtime image query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// list re-fetches server-confirmed state.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { runtimeImageClient } from "@/api/clients";
import type { RuntimeImage } from "@/api/gen/orchicon/api/v1/runtime_image_pb";
import type { PartialMessage } from "@bufbuild/protobuf";

export const runtimeImageKeys = {
  all: ["runtime-images"] as const,
  list: (status?: number, search?: string) =>
    [...runtimeImageKeys.all, "list", status, search] as const,
  detail: (id: string) => [...runtimeImageKeys.all, "detail", id] as const,
  available: ["runtime-images", "available"] as const,
};

// useListRuntimeImages fetches the tenant's runtime image specs.
export function useListRuntimeImages(status?: number, search?: string) {
  return useQuery({
    queryKey: runtimeImageKeys.list(status, search),
    queryFn: async () => {
      const res = await runtimeImageClient.listRuntimeImages({
        status: status ?? undefined,
        search: search || "",
      });
      return res.runtimeImages as RuntimeImage[];
    },
    refetchInterval: 5_000,
  });
}

// useGetRuntimeImage fetches one runtime image spec.
export function useGetRuntimeImage(id: string) {
  return useQuery({
    queryKey: runtimeImageKeys.detail(id),
    queryFn: async () => {
      const res = await runtimeImageClient.getRuntimeImage({ id });
      return res.runtimeImage as RuntimeImage;
    },
    enabled: !!id,
  });
}

// useAvailableRuntimeImages fetches the merged stock + custom image list
// for the work-item runtime_image dropdown.
export function useAvailableRuntimeImages() {
  return useQuery({
    queryKey: runtimeImageKeys.available,
    queryFn: async () => {
      const res = await runtimeImageClient.listAvailableRuntimeImages({});
      return res;
    },
    staleTime: 30_000,
  });
}

// useCreateRuntimeImage saves a new image spec (status=draft).
export function useCreateRuntimeImage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: PartialMessage<import("@/api/gen/orchicon/api/v1/runtime_image_service_pb").CreateRuntimeImageRequest>) => {
      const res = await runtimeImageClient.createRuntimeImage(req);
      return res.runtimeImage as RuntimeImage;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: runtimeImageKeys.all });
    },
  });
}

// useUpdateRuntimeImage edits an image spec (reverts ready -> draft).
export function useUpdateRuntimeImage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: PartialMessage<import("@/api/gen/orchicon/api/v1/runtime_image_service_pb").UpdateRuntimeImageRequest>) => {
      const res = await runtimeImageClient.updateRuntimeImage(req);
      return res.runtimeImage as RuntimeImage;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: runtimeImageKeys.all });
    },
  });
}

// useDeleteRuntimeImage removes the spec row + the local docker image.
export function useDeleteRuntimeImage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await runtimeImageClient.deleteRuntimeImage({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: runtimeImageKeys.all });
    },
  });
}

// useBuildRuntimeImage triggers a docker build on the daemon. The
// mutationFn receives a callback that yields each streamed log chunk;
// it resolves once the build finishes (status ready|failed).
export function useBuildRuntimeImage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (args: {
      id: string;
      version: number;
      onLog: (chunk: string) => void;
    }) => {
      const { id, version, onLog } = args;
      let finalStatus: RuntimeImage["status"] | undefined;
      let finalError = "";
      let finalTag = "";
      for await (const chunk of runtimeImageClient.buildRuntimeImage({ id, version })) {
        if (chunk.log) onLog(chunk.log);
        if (chunk.status !== 0) {
          finalStatus = chunk.status;
          finalError = chunk.error;
          finalTag = chunk.tag;
        }
      }
      if (finalStatus === 4 || finalError) {
        throw new Error(finalError || "image build failed");
      }
      return { status: finalStatus, tag: finalTag };
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: runtimeImageKeys.all });
    },
  });
}
