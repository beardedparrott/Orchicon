// Connect-ES transport and generated service clients.
//
// Per docs/10_Frontend_Architecture.md §3 and AGENTS.md invariant #2,
// the frontend never hand-writes API URLs. Every call goes through the
// generated Connect-ES client imported here. The transport is the single
// place that knows the backend address and injects the tenant header.

import { createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { ProjectService } from "@/api/gen/orchicon/api/v1/project_service_connect";

// TenantHeader is the request header carrying the tenant id. Must match
// internal/middleware/tenant.go.
export const TenantHeader = "x-orchicon-tenant-id";

// Default dev tenant id — matches internal/db/seed.go. The UI sends this
// on every request so the backend's RLS backstop has a tenant scope.
export const DEFAULT_TENANT_ID = "tnt_dev";

// tenantHeaderInterceptor injects the dev tenant header on every RPC.
// Dev-only: when auth lands in Phase 9, this is replaced by an
// Authorization bearer token and the backend derives the tenant from
// the OIDC subject (docs/07 §6).
const tenantHeaderInterceptor: Interceptor = (next) => async (req) => {
  req.header.set(TenantHeader, DEFAULT_TENANT_ID);
  return await next(req);
};

// The dev-server proxy (vite.config.ts) forwards /<package> paths to the
// control plane at :8080. In production the SPA is served alongside the
// control plane (docs/10 §9), so same-origin relative URLs work.
export const connectTransport = createConnectTransport({
  baseUrl:
    typeof window !== "undefined"
      ? window.location.origin
      : "http://localhost:8080",
  interceptors: [tenantHeaderInterceptor],
});

// Typed service client handles. Import these rather than constructing
// clients at call sites. Add services here as the schema grows.
export const projectClient = createClient(ProjectService, connectTransport);
