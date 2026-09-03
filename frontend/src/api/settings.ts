// Settings query hooks (TanStack Query + Connect-ES). Reads/writes
// tenant-level configuration defaults stored in the control plane DB.

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

import { settingsClient } from "@/api/clients";
import type { PartialMessage } from "@bufbuild/protobuf";
import type { TenantSettings } from "@/api/gen/orchicon/api/v1/settings_pb";
import type { BackupEntry, CreateBackupResponse } from "@/api/gen/orchicon/api/v1/settings_service_pb";

export const settingsKeys = {
  all: ["settings"] as const,
  backups: ["backups"] as const,
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
    // PartialMessage (not TenantSettings): every settings tab mutates a
    // few fields at a time, and the server merges non-nil fields —
    // demanding the full class here forced per-site `as any` casts.
    mutationFn: async (settings: PartialMessage<TenantSettings>) => {
      const res = await settingsClient.updateSettings({ settings });
      return res.settings as TenantSettings | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: settingsKeys.all });
    },
  });
}

export function useGetBackups() {
  return useQuery({
    queryKey: settingsKeys.backups,
    queryFn: async () => {
      const res = await settingsClient.listBackups({});
      return res.backups as BackupEntry[];
    },
  });
}

export function useCreateBackup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const res = await settingsClient.createBackup({});
      return res as CreateBackupResponse;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: settingsKeys.backups });
    },
  });
}

export function useRestoreBackup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { name: string }) => {
      await settingsClient.restoreBackup(input);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: settingsKeys.backups });
    },
  });
}

export function useDeleteBackup() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { name: string }) => {
      await settingsClient.deleteBackup(input);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: settingsKeys.backups });
    },
  });
}
