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
import { ApprovalService } from "@/api/gen/orchicon/api/v1/approval_service_connect";
import { AskOrchiconService } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_connect";
import { AuthService } from "@/api/gen/orchicon/api/v1/auth_service_connect";
import { WebhookService } from "@/api/gen/orchicon/api/v1/webhook_service_connect";
import { SettingsService } from "@/api/gen/orchicon/api/v1/settings_service_connect";
import { RuntimeImageService } from "@/api/gen/orchicon/api/v1/runtime_image_service_connect";
import { SecretsService } from "@/api/gen/orchicon/api/v1/secret_service_connect";
import { CategoryService } from "@/api/gen/orchicon/api/v1/category_service_connect";
import { ProviderService } from "@/api/gen/orchicon/api/v1/provider_service_connect";
import { MCPService } from "@/api/gen/orchicon/api/v1/mcp_server_service_connect";
import { getAccessToken, refreshAccessToken } from "@/auth/session";
import type { RefreshResult } from "@/auth/session";

// Refreshing is a module-level guard so concurrent 401s share one
// refresh promise (avoiding a refresh storm).
let refreshInFlight: Promise<RefreshResult> | null = null;

// doRefresh performs a single refresh cycle with single-flight. It
// returns a RefreshResult so the caller can distinguish no-session
// from transient.
async function doRefresh(): Promise<RefreshResult> {
  if (refreshInFlight) {
    return refreshInFlight;
  }
  refreshInFlight = (async () => {
    return await refreshAccessToken();
  })().finally(() => {
    refreshInFlight = null;
  });
  return refreshInFlight;
}

// authInterceptor injects the bearer access token on every RPC. On a
// 401 it transparently refreshes via the HttpOnly cookie and retries
// the call once if the refresh was transient (docs/10 §7). There is
// no pre-login fallback: without a token the call 401s and the app
// shell redirects the user to /login.
const authInterceptor: Interceptor = (next) => async (req) => {
  const token = getAccessToken();
  if (token) {
    req.header.set("Authorization", `Bearer ${token}`);
  }
  try {
    return await next(req);
  } catch (err: unknown) {
    // Connect transmits 401 as a ConnectError with code Unauthenticated.
    const code = (err as { code?: number })?.code ?? (err as { metadata?: { get?: (k: string) => string | null } })?.metadata?.get?.("grpc-status");
    if (code === 16 || code === "16") {
      // Unauthenticated — try one refresh + retry.
      const result = await doRefresh();
      if (result.ok) {
        req.header.set("Authorization", `Bearer ${getAccessToken()}`);
        return await next(req);
      }
      // Transient: retry the original RPC once after a short delay.
      if (result.reason === "transient") {
        await new Promise((r) => setTimeout(r, 1000));
        req.header.set("Authorization", `Bearer ${getAccessToken()}`);
        try {
          return await next(req);
        } catch {
          // Second attempt also failed: throw the original error.
          // The app shell will handle the 401.
        }
      }
      // no-session: throw as today.
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
export const approvalClient = createClient(ApprovalService, connectTransport);
export const authClient = createClient(AuthService, connectTransport);
export const webhookClient = createClient(WebhookService, connectTransport);
export const settingsClient = createClient(SettingsService, connectTransport);
export const askOrchiconClient = createClient(AskOrchiconService, connectTransport);
export const runtimeImageClient = createClient(RuntimeImageService, connectTransport);
export const secretsClient = createClient(SecretsService, connectTransport);
export const categoryClient = createClient(CategoryService, connectTransport);

// Grafana UI base URL for the embedded telemetry explorer (docs/10 §11).
export const GRAFANA_UI_URL = "/grafana";

// Provider settings service (ADR-0006) — Settings → Adapters Providers tab.
export const providerClient = createClient(ProviderService, connectTransport);

// MCP server settings service (ADR-0008) — Settings → Adapters → MCP.
export const mcpClient = createClient(MCPService, connectTransport);


