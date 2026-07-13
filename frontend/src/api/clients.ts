// Connect-ES transport and generated service clients.
//
// Per docs/10_Frontend_Architecture.md §3 and AGENTS.md invariant #2,
// the frontend never hand-writes API URLs. Every call goes through the
// generated Connect-ES client imported here. The transport is the single
// place that knows the backend address and injects the bearer token
// (docs/10 §7). On 401 it transparently refreshes the access token via
// the HttpOnly refresh cookie and retries once.

import { createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { ProjectService } from "@/api/gen/orchicon/api/v1/project_service_connect";
import { WorkerService } from "@/api/gen/orchicon/api/v1/worker_service_connect";
import { WorkItemService } from "@/api/gen/orchicon/api/v1/work_item_service_connect";
import { RuntimeAdapterService } from "@/api/gen/orchicon/api/v1/adapter_service_connect";
import { ExecutionService } from "@/api/gen/orchicon/api/v1/execution_service_connect";
import { WorkflowService } from "@/api/gen/orchicon/api/v1/workflow_service_connect";
import { PolicyService } from "@/api/gen/orchicon/api/v1/policy_service_connect";
import { RecoveryService } from "@/api/gen/orchicon/api/v1/recovery_service_connect";
import { TelemetryService } from "@/api/gen/orchicon/api/v1/telemetry_service_connect";
import { AIGatewayService } from "@/api/gen/orchicon/api/v1/ai_gateway_service_connect";
import { AuthService } from "@/api/gen/orchicon/api/v1/auth_service_connect";
import { WebhookService } from "@/api/gen/orchicon/api/v1/webhook_service_connect";
import { getAccessToken, refreshAccessToken } from "@/auth/session";

// TenantHeader is retained for the dev pre-login fallback (when the
// backend has not yet minted a token, it accepts the dev tenant header).
export const TenantHeader = "x-orchicon-tenant-id";
export const DEFAULT_TENANT_ID = "tnt_dev";

// Refreshing is a module-level guard so concurrent 401s share one
// refresh promise (avoiding a refresh storm).
let refreshInFlight: Promise<boolean> | null = null;

async function doRefresh(): Promise<boolean> {
  if (refreshInFlight) {
    return refreshInFlight;
  }
  refreshInFlight = (async () => {
    const session = await refreshAccessToken();
    return !!session?.authenticated;
  })().finally(() => {
    refreshInFlight = null;
  });
  return refreshInFlight;
}

// authInterceptor injects the bearer access token on every RPC. On a
// 401 it transparently refreshes via the HttpOnly cookie and retries
// the call once (docs/10 §7). Dev pre-login (no token) falls back to
// the dev tenant header so the UI works before authentication.
const authInterceptor: Interceptor = (next) => async (req) => {
  const token = getAccessToken();
  if (token) {
    req.header.set("Authorization", `Bearer ${token}`);
  } else {
    // Dev pre-login fallback: the backend resolves the dev tenant.
    req.header.set(TenantHeader, DEFAULT_TENANT_ID);
  }
  try {
    return await next(req);
  } catch (err: unknown) {
    // Connect transmits 401 as a ConnectError with code Unauthenticated.
    const code = (err as { code?: number })?.code ?? (err as { metadata?: { get?: (k: string) => string | null } })?.metadata?.get?.("grpc-status");
    if (code === 16 || code === "16") {
      // Unauthenticated — try one refresh + retry.
      const ok = await doRefresh();
      if (ok) {
        req.header.set("Authorization", `Bearer ${getAccessToken()}`);
        return await next(req);
      }
    }
    throw err;
  }
};

export const connectTransport = createConnectTransport({
  baseUrl:
    typeof window !== "undefined"
      ? window.location.origin
      : "http://localhost:8080",
  interceptors: [authInterceptor],
  credentials: "include",
});

export const projectClient = createClient(ProjectService, connectTransport);
export const workerClient = createClient(WorkerService, connectTransport);
export const workItemClient = createClient(WorkItemService, connectTransport);
export const adapterClient = createClient(RuntimeAdapterService, connectTransport);
export const executionClient = createClient(ExecutionService, connectTransport);
export const workflowClient = createClient(WorkflowService, connectTransport);
export const policyClient = createClient(PolicyService, connectTransport);
export const recoveryClient = createClient(RecoveryService, connectTransport);
export const telemetryClient = createClient(TelemetryService, connectTransport);
export const aiGatewayClient = createClient(AIGatewayService, connectTransport);
export const authClient = createClient(AuthService, connectTransport);
export const webhookClient = createClient(WebhookService, connectTransport);

// SigNoz UI base URL for the embedded telemetry explorer (docs/10 §11).
export const SIGNOZ_UI_URL = "/signoz";
