/// <reference types="vitest" />
import path from "node:path";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { TanStackRouterVite } from "@tanstack/router-plugin/vite";

/**
 * vite.activity-e2e.config.ts — the SPA dev server for the activity-line capstone's GUI leg.
 *
 * It is vite.config.ts with ONE difference: the Connect API prefix and the auth routes proxy to the
 * FIXTURE PLANE on :18080 instead of the default :8080. The default port belongs to the container's
 * own sandbox plane, which has no agent behind it and whose conversation is not the fixture's — so
 * proxying there would make every observation a statement about the WRONG plane.
 *
 * Port 5174 (not 5173): a dev server someone else left running on 5173 must not be silently reused.
 */
const target = process.env.E2E_PLANE_TARGET || "http://127.0.0.1:18080";

export default defineConfig({
  test: { exclude: ["node_modules/**", "tests/**", "dist/**"] },
  plugins: [
    TanStackRouterVite({ target: "react", autoCodeSplitting: true }),
    react(),
  ] as unknown as never,
  resolve: { alias: { "@": path.resolve(__dirname, "./src") } },
  server: {
    port: 5174,
    proxy: {
      "/orchicon.api.v1": { target, changeOrigin: true },
      "/auth": { target, changeOrigin: true },
    },
  },
});
