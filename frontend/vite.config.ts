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
      // Grafana UI proxy (docs/10 §11): seamless embedding — the Grafana
      // iframe is served same-origin under /grafana so it shares the
      // Orchicon shell's auth + visual language, not a separate tool.
      // Grafana runs with serve_from_sub_path so it generates every asset
      // and API URL under /grafana itself; proxy through the Go control
      // plane so the /grafana reverse proxy strips the prefix and forwards
      // (docs/10 §11).
      "/grafana": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});