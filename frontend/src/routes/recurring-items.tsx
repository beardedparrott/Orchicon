import { createRoute } from "@tanstack/react-router";
import { Link } from "@tanstack/react-router";

import { Route as rootRoute } from "@/routes/__root";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Repeat, Calendar } from "lucide-react";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recurring-items",
  component: RecurringItemsPage,
});

function RecurringItemsPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2">
          <Repeat className="w-6 h-6 text-fuchsia-400" />
          Recurring Items
          <span className="text-[10px] bg-cyan-500/20 text-cyan-300 px-1.5 py-0.5 rounded border border-cyan-500/30 font-semibold">
            NEW
          </span>
        </h1>
        <p className="text-sm text-muted-foreground">
          Special work-item type living under Automation → Recurring Items. Regular Work Items no
          longer expose the recurring schedule option.
        </p>
      </div>

      <Card className="glass-panel">
        <CardHeader>
          <CardTitle className="text-base">About Recurring Items</CardTitle>
          <CardDescription>
            Recurring Items are a special work-item type that runs on a schedule, separate from
            one-off Work Items.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4 text-sm text-muted-foreground">
          <p>
            This is the dedicated entry point required by the new top-nav Automation → Recurring
            Items link. The full recurring scheduling UI (frequency, next run, enable/disable) will
            land as a follow-on to Task 2; until then, this page preserves the deep link and
            explains the split so bookmarks and nav links do not 404.
          </p>
          <div className="flex gap-2">
            <Button asChild variant="outline" size="sm">
              <Link to="/work-items">
                <Calendar className="w-4 h-4 mr-1" />
                View Work Items
              </Link>
            </Button>
            <Button asChild variant="outline" size="sm">
              <Link to="/schedules">
                <Calendar className="w-4 h-4 mr-1" />
                View Schedules
              </Link>
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
