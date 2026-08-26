import { createFileRoute, redirect } from "@tanstack/react-router";
export const Route = createFileRoute("/conversations")({ beforeLoad: () => { throw redirect({ to: "/ask-orchicon", search: { conversationId: null } as never }); }, component: () => null });
