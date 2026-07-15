import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { ConnectError } from "@connectrpc/connect";

import { router } from "@/router";
import { AuthProvider } from "@/auth/auth";
import { ThemeProvider } from "@/components/theme-provider";
import { Toaster } from "@/components/ui/toaster";
import { useToastStore } from "@/components/ui/toast";
import "@/index.css";

// Extract a human-readable error from whatever the transport threw. We
// see ConnectError in 99% of cases (Connect-ES wraps every RPC failure
// in one); fall back to the raw message for anything else (e.g. fetch
// network errors, AbortError).
function describeError(e: unknown): string {
  if (e instanceof ConnectError) {
    return e.rawMessage || e.message || `code=${e.code}`;
  }
  if (e instanceof Error) return e.message;
  return String(e);
}

// TanStack Query holds server state; UI-only state lives in Zustand
// (docs/10 §6). The cache is invalidated by mutations and stream events.
//
// defaultOptions.mutations.onError surfaces every uncaught mutation
// failure as a toast — pre-toast, the only feedback was a silent
// network call and a UI that didn't update, which made every "Create"
// / "Save" button in the admin look broken. Per-mutation handlers can
// still override or add a success toast on top.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      refetchOnWindowFocus: false,
      retry: 1,
    },
    mutations: {
      onError: (err) => {
        useToastStore.getState().push({
          kind: "error",
          message: describeError(err),
        });
      },
    },
  },
});

const root = document.getElementById("root");
if (!root) throw new Error("#root not found");

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <ThemeProvider>
          <RouterProvider router={router} />
          <Toaster />
        </ThemeProvider>
      </AuthProvider>
    </QueryClientProvider>
  </StrictMode>
);
