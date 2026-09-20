import { createRoute } from "@tanstack/react-router";

import { PoliciesComingSoon } from "@/components/policies/ComingSoon";
import { Route as rootRoute } from "@/routes/__root";

// Policies list (docs/10 §5, docs/02 §2.5).
//
// The ROUTE stays registered — the nav links to /policies, and an unregistered route would
// 404 instead of telling the operator anything. The page itself is a "coming soon"
// placeholder while the editor design is unsettled (see PoliciesComingSoon).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies",
  component: PoliciesComingSoon,
});
