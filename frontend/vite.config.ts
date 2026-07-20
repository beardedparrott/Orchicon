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
      // SigNoz UI proxy (docs/10 §11): seamless embedding — the SigNoz
      // iframe is served same-origin under /signoz so it shares the
      // Orchicon shell's auth + visual language, not a separate tool.
      // Proxy through the Go control plane so the ModifyResponse applied
      // by the /signoz reverse proxy rewrites the HTML's <base href> and
      // asset paths to include the /signoz/ prefix (docs/10 §11, AGENTS.md
      // verification: blank-iframe regression). Direct-to-SigNoz proxying
      // would bypass this rewrite and cause MIME-type errors on assets.
      "/signoz": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
