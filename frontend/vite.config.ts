import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { TanStackRouterVite } from "@tanstack/router-plugin/vite";

// https://vite.dev/config/
// Per docs/10_Frontend_Architecture.md §9, the dev server proxies to the
// control plane; in production the SPA is served by the control-plane
// binary or a CDN.
export default defineConfig({
  plugins: [
    TanStackRouterVite({ target: "react", autoCodeSplitting: true }),
    react(),
  ],
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
