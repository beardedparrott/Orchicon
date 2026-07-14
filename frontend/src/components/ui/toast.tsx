import { create } from "zustand";

// Minimal toast store + hook. The UI primitives stay lean per
// AGENTS.md invariant #1; the toaster lives in components/ui/toaster.tsx
// and the styles in index.css. Auto-dismiss is handled inside the
// Toaster component, not here, so the store stays free of timers.

export type ToastKind = "success" | "error" | "info";

export type Toast = {
  id: string;
  kind: ToastKind;
  title?: string;
  message: string;
  /** milliseconds; 0 means "do not auto-dismiss" */
  duration: number;
};

type ToastState = {
  toasts: Toast[];
  push: (t: Omit<Toast, "id">) => string;
  dismiss: (id: string) => void;
  clear: () => void;
};

let counter = 0;
const nextId = () => `t${Date.now()}_${++counter}`;

export const useToastStore = create<ToastState>((set) => ({
  toasts: [],
  push: (t) => {
    const id = nextId();
    set((s) => ({ toasts: [...s.toasts, { id, ...t }] }));
    return id;
  },
  dismiss: (id) =>
    set((s) => ({ toasts: s.toasts.filter((x) => x.id !== id) })),
  clear: () => set({ toasts: [] }),
}));

// useToast returns a small ergonomic API for the rest of the app.
export function useToast() {
  const push = useToastStore((s) => s.push);
  return {
    success: (message: string, opts?: { title?: string; duration?: number }) =>
      push({ kind: "success", message, title: opts?.title, duration: opts?.duration ?? 4000 }),
    error: (message: string, opts?: { title?: string; duration?: number }) =>
      push({ kind: "error", message, title: opts?.title, duration: opts?.duration ?? 6000 }),
    info: (message: string, opts?: { title?: string; duration?: number }) =>
      push({ kind: "info", message, title: opts?.title, duration: opts?.duration ?? 4000 }),
  };
}
