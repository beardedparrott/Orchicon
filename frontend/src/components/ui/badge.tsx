import { cn } from "@/lib/utils";

interface BadgeProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?: "default" | "secondary" | "destructive" | "outline";
}

function Badge({ className, variant = "default", ...props }: BadgeProps) {
  return (
    <div
      className={cn(
        "inline-flex items-center rounded-md border px-2.5 py-0.5 text-xs font-semibold transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2",
        variant === "default" && "border border-[hsla(var(--glass-panel-border)/var(--glass-panel-border-a))] bg-primary text-primary-foreground shadow-sm backdrop-blur-sm",
        variant === "secondary" && "border border-[hsla(var(--glass-panel-border)/var(--glass-panel-border-a))] bg-white/5 backdrop-blur-sm text-secondary-foreground",
        variant === "destructive" && "border border-destructive/30 bg-destructive text-destructive-foreground shadow-sm",
        variant === "outline" && "border-[hsla(var(--glass-panel-border)/var(--glass-panel-border-a))] bg-white/5 backdrop-blur-sm text-foreground",
        className,
      )}
      {...props}
    />
  );
}

export { Badge };
