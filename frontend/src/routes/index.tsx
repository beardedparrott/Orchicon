import { createRoute, redirect } from "@tanstack/react-router";

import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/ask-orchicon", search: { conversationId: null } } as never);
  },
  component: () => null,
});
