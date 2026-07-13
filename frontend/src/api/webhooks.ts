// WebhookService query + mutation hooks (TanStack Query + Connect-ES).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { webhookClient } from "@/api/clients";

export const webhookKeys = {
  subs: () => ["webhooks", "subs"] as const,
  deliveries: (subId?: string) => ["webhooks", "deliveries", subId ?? ""] as const,
};

export function useListSubscriptions() {
  return useQuery({
    queryKey: webhookKeys.subs(),
    queryFn: async () =>
      (await webhookClient.listSubscriptions({ pageSize: 100 })).subscriptions ?? [],
  });
}

export function useCreateSubscription() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      name: string;
      targetUrl: string;
      eventFilter: string;
      scope?: string;
      scopeRef?: string;
      secret?: string;
      maxRetries?: number;
    }) => (await webhookClient.createSubscription(input)).subscription,
    onSuccess: () => qc.invalidateQueries({ queryKey: webhookKeys.subs() }),
  });
}

export function useDeleteSubscription() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => await webhookClient.deleteSubscription({ id }),
    onSuccess: () => qc.invalidateQueries({ queryKey: webhookKeys.subs() }),
  });
}

export function useTestSubscription() {
  return useMutation({
    mutationFn: async (id: string) =>
      (await webhookClient.testSubscription({ id })).delivery,
  });
}

export function useListDeliveries(subId?: string) {
  return useQuery({
    queryKey: webhookKeys.deliveries(subId),
    queryFn: async () =>
      (await webhookClient.listDeliveries({ pageSize: 100, subscriptionId: subId ?? "" })).deliveries ?? [],
  });
}

export function useReplayDelivery() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) =>
      (await webhookClient.replayDelivery({ id })).delivery,
    onSuccess: () => qc.invalidateQueries({ queryKey: webhookKeys.deliveries() }),
  });
}
