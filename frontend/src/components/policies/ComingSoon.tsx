import { Construction } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

/**
 * The Policies surface, while it is not wired up.
 *
 * The operator: "Policies. We never really ironed these out. They exist in the gui but the
 * form is weird and I have never tested it. I would like both the TUI and the GUI to just say
 * 'Coming soon...' for now."
 *
 * ONE component for all three policy routes (/policies, /policies/new, /policies/$id) so they
 * cannot disagree about the state of the feature — the same reason the TUI shares
 * `ComingSoonText` with this string. The wording is deliberately identical to the TUI's.
 */
export const COMING_SOON_TEXT = "Coming soon...";

export function PoliciesComingSoon() {
  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2">
            <span className="inline-flex h-2 w-2 rounded-full bg-indigo-400 animate-pulse motion-reduce:animate-none" />
            Policies
          </h1>
          <p className="text-sm text-muted-foreground">
            Rego-based decision-point policies (Tier&nbsp;1). Evaluate at admission, dispatch,
            budget, approval, recovery, and completion.
          </p>
        </div>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Construction aria-hidden="true" className="h-5 w-5 text-muted-foreground" />
            {COMING_SOON_TEXT}
          </CardTitle>
          <CardDescription>
            Policies are not available yet. The editor has not been through review, so the
            surface is deliberately inert rather than half-working — nothing here writes to
            the plane.
          </CardDescription>
        </CardHeader>
        <CardContent className="text-sm text-muted-foreground">
          The capability itself is untouched: the policy RPCs and the stored policies are
          unchanged. Only this UI is parked until the design is settled.
        </CardContent>
      </Card>
    </div>
  );
}
