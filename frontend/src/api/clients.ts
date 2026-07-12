// Connect-ES transport and generated service clients.
//
// Per docs/10_Frontend_Architecture.md §3 and AGENTS.md invariant #2,
// the frontend never hand-writes API URLs. Every call goes through the
// generated Connect-ES client imported here. The transport is the single
// place that knows the backend address.

import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { ProjectService } from "@/api/gen/orchicon/api/v1/project_service_connect";

// The dev-server proxy (vite.config.ts) forwards /<package> paths to the
// control plane at :8080. In production the SPA is served alongside the
// control plane (docs/10 §9), so same-origin relative URLs work.
export const connectTransport = createConnectTransport({
  baseUrl:
    typeof window !== "undefined"
      ? window.location.origin
      : "http://localhost:8080",
});

// Typed service client handles. Import these rather than constructing
// clients at call sites. Add services here as the schema grows.
export const projectClient = createClient(ProjectService, connectTransport);
