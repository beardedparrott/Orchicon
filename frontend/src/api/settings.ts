// Settings query hooks (TanStack Query + Connect-ES). Reads/writes
// tenant-level configuration defaults stored in the control plane DB.

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { settingsClient } from "@/api/clients";
import type { TenantSettings } from "@/api/gen/orchicon/api/v1/settings_pb";

export const settingsKeys = {
  all: ["settings"] as const,
};

export function useGetSettings() {
  return useQuery({
    queryKey: settingsKeys.all,
    queryFn: async () => {
      const res = await settingsClient.getSettings({});
      return res.settings as TenantSettings | undefined;
    },
  });
}

export function useUpdateSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (settings: TenantSettings) => {
      const res = await settingsClient.updateSettings({ settings });
      return res.settings as TenantSettings | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: settingsKeys.all });
    },
  });
}
