import { useEffect } from "react";

import { cn } from "@/lib/utils";
import { useToastStore, type Toast } from "@/components/ui/toast";

// <Toaster /> renders the active toasts and handles auto-dismiss.
// Mounted once near the root (see main.tsx). Reads the toast store
// directly so toasts are reactive without prop-drilling.

const DEFAULT_DURATIONS: Record<NonNullable<Toast["kind"]>, number> = {
  success: 4000,
  error: 6000,
  info: 4000,
};

export function Toaster() {
  const toasts = useToastStore((s) => s.toasts);
  const dismiss = useToastStore((s) => s.dismiss);

  return (
    <div
      className="pointer-events-none fixed bottom-4 right-4 z-50 flex w-full max-w-sm flex-col gap-2"
      aria-live="polite"
      aria-atomic="true"
    >
      {toasts.map((t) => (
        <ToastItem key={t.id} toast={t} onDismiss={() => dismiss(t.id)} />
      ))}
    </div>
  );
}

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: () => void }) {
  // Auto-dismiss. duration=0 sticks; absent duration falls back to a
  // per-kind default so raw push() calls (e.g. the global onError in
  // main.tsx) auto-dismiss without the caller having to think about it.
  const duration = toast.duration ?? DEFAULT_DURATIONS[toast.kind];
  useEffect(() => {
    if (duration <= 0) return;
    const id = window.setTimeout(onDismiss, duration);
    return () => window.clearTimeout(id);
  }, [duration, onDismiss]);

  return (
    <div
      role={toast.kind === "error" ? "alert" : "status"}
      className={cn(
        "pointer-events-auto rounded-md border bg-card p-3 shadow-lg",
        "flex items-start gap-3",
        toast.kind === "success" && "border-emerald-500/40",
        toast.kind === "error" && "border-red-500/40",
        toast.kind === "info" && "border-sky-500/40"
      )}
    >
      <span
        aria-hidden
        className={cn(
          "mt-1 inline-block h-2 w-2 shrink-0 rounded-full",
          toast.kind === "success" && "bg-emerald-500",
          toast.kind === "error" && "bg-red-500",
          toast.kind === "info" && "bg-sky-500"
        )}
      />
      <div className="min-w-0 flex-1">
        {toast.title && <div className="text-sm font-medium">{toast.title}</div>}
        <div className="text-sm text-muted-foreground break-words">
          {toast.message}
        </div>
      </div>
      <button
        type="button"
        onClick={onDismiss}
        className="rounded-md p-1 text-muted-foreground hover:text-foreground"
        aria-label="Dismiss"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 20 20"
          fill="currentColor"
          className="h-4 w-4"
        >
          <path
            fillRule="evenodd"
            d="M4.293 4.293a1 1 0 011.414 0L10 8.586l4.293-4.293a1 1 0 111.414 1.414L11.414 10l4.293 4.293a1 1 0 01-1.414 1.414L10 11.414l-4.293 4.293a1 1 0 01-1.414-1.414L8.586 10 4.293 5.707a1 1 0 010-1.414z"
            clipRule="evenodd"
          />
        </svg>
      </button>
    </div>
  );
}
