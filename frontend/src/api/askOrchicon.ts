import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { askOrchiconClient } from "@/api/clients";
import type { Conversation } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import type { ChatMessage } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";

export const askKeys = {
  conversations: ["ask", "conversations"] as const,
  conversation: (id: string) => ["ask", "conversation", id] as const,
  messages: (id: string) => ["ask", "messages", id] as const,
  config: ["ask", "config"] as const,
};

export function useListConversations() {
  return useQuery({
    queryKey: askKeys.conversations,
    queryFn: async () => {
      const res = await askOrchiconClient.listConversations({ pageSize: 50 });
      return (res.conversations ?? []) as Conversation[];
    },
  });
}

export function useGetConversation(id: string) {
  return useQuery({
    queryKey: askKeys.conversation(id),
    queryFn: async () => {
      const res = await askOrchiconClient.getConversation({ id });
      return res.conversation as Conversation | undefined;
    },
    enabled: !!id,
  });
}

export function useCreateConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { modelRef?: string; initialMessage?: string }) => {
      const res = await askOrchiconClient.createConversation({
        modelRef: opts.modelRef ?? "",
        initialMessage: opts.initialMessage ?? "",
      });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useDeleteConversation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await askOrchiconClient.deleteConversation({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useUpdateConversationTitle() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (opts: { id: string; title: string }) => {
      const res = await askOrchiconClient.updateConversationTitle({ id: opts.id, title: opts.title });
      return res.conversation as Conversation | undefined;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.conversations });
    },
  });
}

export function useListMessages(conversationId: string) {
  return useQuery({
    queryKey: askKeys.messages(conversationId),
    queryFn: async () => {
      const res = await askOrchiconClient.listMessages({ conversationId, pageSize: 200 });
      return (res.messages ?? []).reverse() as ChatMessage[];
    },
    enabled: !!conversationId,
  });
}

export function useGetAgentConfig() {
  return useQuery({
    queryKey: askKeys.config,
    queryFn: async () => {
      const res = await askOrchiconClient.getAgentConfig({});
      return res.config;
    },
  });
}

export function useUpdateAgentConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (config: any) => {
      const res = await askOrchiconClient.updateAgentConfig({ config });
      return res.config;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: askKeys.config });
    },
  });
}
