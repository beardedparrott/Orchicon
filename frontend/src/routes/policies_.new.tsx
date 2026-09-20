import { createRoute } from "@tanstack/react-router";

import { PoliciesComingSoon } from "@/components/policies/ComingSoon";
import { Route as rootRoute } from "@/routes/__root";

// Create policy form (docs/02 §2.5).
//
// Placeholder: the operator has never been able to test this form ("the form is weird and I
// have never tested it"), so the route renders "Coming soon..." rather than a create form that
// writes to the plane. The route stays registered so a bookmark or a stale link explains
// itself instead of 404-ing.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/policies/new",
  component: PoliciesComingSoon,
});
