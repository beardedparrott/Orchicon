import { Link, useRouterState } from "@tanstack/react-router";
import { getBreadcrumbs } from "@/lib/nav-config";
type BreadcrumbsProps = { detail?: { label: string; to?: string } | null; };
export function Breadcrumbs({ detail }: BreadcrumbsProps) {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const crumbs = getBreadcrumbs(path);
  if (crumbs.length === 0 && !detail) return null;
  const all = detail ? [...crumbs, detail] : crumbs;
  return (
    <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground flex items-center gap-1 flex-wrap">
      {all.map((c, idx) => {
        const isLast = idx === all.length - 1;
        return (
          <span key={`${c.label}-${idx}`} className="flex items-center gap-1">
            {idx > 0 && <span aria-hidden="true" className="text-muted-foreground/60">›</span>}
            {c.to && !isLast ? <Link to={c.to} className="hover:text-foreground hover:underline">{c.label}</Link> : isLast ? <span className="text-foreground font-medium">{c.label}</span> : <span>{c.label}</span>}
          </span>
        );
      })}
    </nav>
  );
}
