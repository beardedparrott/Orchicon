import { createRoute } from "@tanstack/react-router";

import { PoliciesComingSoon } from "@/components/policies/ComingSoon";
import { Route as rootRoute } from "@/routes/__root";

// Policy detail: versions, the Rego body, publish/supersede, and the evaluate-against-input
// sandbox.
//
// Placeholder: the whole editor is parked while the design is unsettled (the operator has
// never been able to test the form). The route stays registered — deep links to a policy exist
// in bookmarks and in decisions — so it explains itself instead of 404-ing.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/$id",
  component: PoliciesComingSoon,
});
