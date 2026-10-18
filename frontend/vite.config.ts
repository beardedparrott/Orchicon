/// <reference types="vitest" />
import path from "node:path";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { TanStackRouterVite } from "@tanstack/router-plugin/vite";

// https://vite.dev/config/
// Per docs/10_Frontend_Architecture.md §9, the dev server proxies to the
// control plane; in production the SPA is served by the control-plane
// binary or a CDN.
//
// defineConfig comes from vitest/config — the same config surface vite
// accepts, plus the `test` block, in ONE file (the officially documented
// pattern). The plugins array is cast through `unknown` because vitest
// vendors its own vite copy (peer range ^5 vs the app's 6.x); the nested
// copy makes cross-package Plugin types structurally incompatible here —
// type-only friction, no runtime difference.
//
// The `test` block scopes vitest to unit/component tests ONLY:
// tests/*.spec.ts are Playwright E2E specs (playwright.config.ts +
// npm run test:snapshots / test:a11y / test:scope). Without the exclude,
// vitest's default **/*.spec.ts include sweeps them in and fails at
// collection ("Playwright Test did not expect test() to be called here"),
// polluting `npm test` with false-positive suite failures.
export default defineConfig({
  test: {
    exclude: ["node_modules/**", "tests/**", "dist/**"],
  },
  plugins: [
    TanStackRouterVite({ target: "react", autoCodeSplitting: true }),
    react(),
  ] as unknown as never,
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Operational status endpoints the SPA reads as plain JSON (they sit
      // beside /healthz and /versionz on the control plane's mux, not under
      // the Connect prefix). WITHOUT THIS RULE a dev-server fetch("/enginez")
      // never reaches the plane: Vite answers it with the SPA's index.html —
      // HTTP 200 and HTML, so the execution-mode availability badge reports
      // "availability unknown" on a host where containers are perfectly fine.
      "/enginez": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      // Connect-ES + gRPC-Web share the same path prefix.
      "/orchicon.api.v1": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      // Auth endpoints (local-login/refresh/oidc/session) + the embedded
      // OpenID Provider login bridge — proxied so the SPA login flow
      // works under make fe-dev.
      "/auth": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      // Embedded OpenID Provider endpoints (internal/auth/op): a dev
      // RP pointed at the plane origin needs these to reach the Go
      // control plane during development.
      "/.well-known/openid-configuration": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      "/authorize": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      "/token": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      "/userinfo": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      "/jwks": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});