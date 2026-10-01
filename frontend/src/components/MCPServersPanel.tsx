// MCP servers panel (ADR-0008, epic: platform model). ONE component serves
// every placement — project, Ask conversation and worker version — because a
// capability that lands in one scope and not another is incomplete work.
//
// It is the extracted, SCOPE-PARAMETERIZED form of the former
// MCPServersTab: the Add button + runtime hint, the stdio-vs-streamable-HTTP
// add/edit form, the Configured servers cards, the Registry catalog grid
// with one-click prefill, and the write-only Credentials form are all kept
// intact. What changed is the FRAMING:
//
//   - "Configured servers" now means THE ENTRIES THIS SCOPE OWNS. Each is a
//     project-owned or conversation-owned row (or, at the worker-version
//     scope, an inline spec written into the version's permissions JSON).
//   - There is no tenant default and no tenant-level selector: the owner
//     column IS the selection.
//   - Credentials stay tenant-scoped on purpose — the secrets store is
//     tenant-scoped because RLS needs a tenant, and that is a credential
//     store, not an MCP scope.
//
// The honest consequence is stated rather than hidden: a server can no
// longer be defined once and inherited by several projects. The catalog's
// one-click Add per scope is the mitigation.
import { useState } from "react";

import {
  useMCPServerList,
  useMCPCatalog,
  useMCPRuntimes,
  useCreateMCPServer,
  useUpdateMCPServer,
  useDeleteMCPServer,
  useInstallMCPServer,
  useSetMCPServerSecret,
  usePrefillMCPCatalogEntry,
} from "@/api/mcpServers";
import { MCPServerTransport } from "@/api/gen/orchicon/api/v1/mcp_server_pb";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Cable,
  CheckCircle2,
  KeyRound,
  Plus,
  Rocket,
  Trash2,
  XCircle,
} from "lucide-react";

const TRANSPORT_LABEL: Record<number, string> = {
  [MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO]: "stdio",
  [MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP]: "streamable HTTP",
};

// InlineMCP mirrors db.InlineMCPServer (internal/db/mcp_servers.go) — the
// ONE shape the server already parses out of a worker version's
// permissions.mcp_servers. The worker-version placement writes THIS array
// rather than an owned row, so a published version stays immutable.
export interface InlineMCP {
  id: string;
  type?: string; // "stdio" | "http"
  command?: string[];
  url?: string;
  headers?: Record<string, string>;
  env?: Record<string, string>;
  enabled?: boolean;
  onError?: string;
}

// MCPScope is the panel's only new axis: WHERE the entries live and HOW
// they persist. project/conversation persist owned rows through the MCP
// service; workerVersion persists inline specs through the caller's onChange.
export type MCPScope =
  | { kind: "project"; projectId: string }
  | { kind: "conversation"; conversationId: string }
  | { kind: "workerVersion"; value: InlineMCP[]; onChange: (next: InlineMCP[]) => void };

export interface MCPServersPanelProps {
  scope: MCPScope;
  readOnly?: boolean;
  /** The project whose servers this scope inherits. Rendered read-only,
   *  labelled with the source, above the scope's own additions. */
  inheritedFrom?: { projectId: string; projectName: string };
}

interface FormState {
  name: string;
  transport: MCPServerTransport;
  command: string;
  args: string;
  env: string;
  url: string;
  headers: string;
  enabled: boolean;
}

const emptyForm: FormState = {
  name: "",
  transport: MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO,
  command: "",
  args: "",
  env: "",
  url: "",
  headers: "",
  enabled: true,
};

function parseKeyValue(input: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of input.split("\n")) {
    const idx = line.indexOf("=");
    if (idx > 0) {
      out[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
    }
  }
  return out;
}

function keyValueText(map: Record<string, string> | undefined): string {
  if (!map) return "";
  return Object.entries(map)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

// A display row is the common shape both persistence modes render into, so
// the Configured-servers card markup is written once.
interface Row {
  id: string;
  name: string;
  transport: number;
  catalogSlug: string;
  enabled: boolean;
  command?: string;
  args?: string[];
  // env/headers are carried so the EDIT form can show what the entry already
  // has. The update sends replaceEnv/replaceHeaders, so an edit that started
  // from empty fields would ERASE them — the env/headers must round-trip.
  env?: Record<string, string>;
  headers?: Record<string, string>;
  url?: string;
  installStatus: number;
  hasSecretStored: boolean;
  installResult?: { ok?: boolean; error?: string } | null;
}

function ownedRow(s: {
  id: string;
  name: string;
  transport: number;
  catalogSlug?: string;
  enabled: boolean;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  headers?: Record<string, string>;
  url?: string;
  installStatus: number;
  hasSecretStored?: boolean;
  installResult?: { ok?: boolean; error?: string } | null;
}): Row {
  return {
    id: s.id,
    name: s.name,
    transport: s.transport,
    catalogSlug: s.catalogSlug ?? "",
    enabled: s.enabled,
    command: s.command,
    args: s.args,
    env: s.env,
    headers: s.headers,
    url: s.url,
    installStatus: s.installStatus,
    hasSecretStored: !!s.hasSecretStored,
    installResult: s.installResult,
  };
}

function inlineRow(s: InlineMCP): Row {
  const stdio = (s.type ?? "stdio") !== "http";
  return {
    id: s.id,
    name: s.id,
    transport: stdio
      ? MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO
      : MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP,
    catalogSlug: "",
    enabled: s.enabled ?? true,
    command: stdio ? (s.command ?? [])[0] : undefined,
    args: stdio ? (s.command ?? []).slice(1) : undefined,
    env: s.env,
    headers: s.headers,
    url: stdio ? undefined : s.url,
    // An inline worker spec has no row, so it has no install status.
    installStatus: 0,
    hasSecretStored: false,
  };
}

function InstallStatusBadge({ row }: { row: Row }) {
  const s = row.installStatus;
  if (s === 4) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-emerald-500">
        <CheckCircle2 className="h-3 w-3" /> installed
      </span>
    );
  }
  if (s === 5) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-destructive" title={row.installResult?.error ?? ""}>
        <XCircle className="h-3 w-3" /> failed
      </span>
    );
  }
  if (s === 3) {
    return <span className="text-xs text-muted-foreground">installing…</span>;
  }
  return <span className="text-xs text-muted-foreground">not installed</span>;
}

// InheritedServers shows the PROJECT's contributed servers READ-ONLY, naming
// the source, so an operator can always answer "why is this server available
// here?". The owner filter IS the inheritance query — no new RPC.
function InheritedServers({ projectId, projectName }: { projectId: string; projectName: string }) {
  const { data: servers = [] } = useMCPServerList({ projectId });
  if (servers.length === 0) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>Inherited from project {projectName}</CardTitle>
        <CardDescription>
          These servers belong to the project and are available here
          automatically. They are read-only at this scope — edit them on the
          project page.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        {servers.map((s) => (
          <div key={s.id} className="flex flex-wrap items-center gap-3 rounded-md border border-white/10 p-3 opacity-80">
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <span className="font-medium">{s.name}</span>
                <span className="text-xs text-muted-foreground">
                  {TRANSPORT_LABEL[s.transport] ?? "?"}
                </span>
                <span className="text-xs text-muted-foreground">from project</span>
              </div>
              <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                <span className={s.enabled ? "text-emerald-500" : ""}>{s.enabled ? "enabled" : "disabled"}</span>
                {s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO && s.command && (
                  <span className="truncate font-mono">{s.command} {(s.args ?? []).join(" ")}</span>
                )}
                {s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP && s.url && (
                  <span className="truncate font-mono">{s.url}</span>
                )}
              </div>
            </div>
            <span className="text-xs text-muted-foreground">read-only</span>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

export function MCPServersPanel({ scope, readOnly = false, inheritedFrom }: MCPServersPanelProps) {
  const owned = scope.kind !== "workerVersion";
  const scopeFilter =
    scope.kind === "project"
      ? { projectId: scope.projectId }
      : scope.kind === "conversation"
        ? { conversationId: scope.conversationId }
        : undefined;

  // In the worker-version mode there is no list query AT ALL: the entries live
  // in the caller's array (the version's permissions JSON). It is disabled
  // rather than merely unused because an unscoped ListMCPServers IS the
  // whole-tenant list the epic removed — a query that fires and is then ignored
  // would still be a live tenant surface.
  const { data: listed = [], isLoading, error } = useMCPServerList(
    owned ? scopeFilter : undefined,
    { enabled: owned },
  );
  const { data: catalog = [] } = useMCPCatalog();
  const { data: runtimes } = useMCPRuntimes();

  const createServer = useCreateMCPServer();
  const updateServer = useUpdateMCPServer();
  const deleteServer = useDeleteMCPServer();
  const installServer = useInstallMCPServer();
  const setSecret = useSetMCPServerSecret();
  const prefill = usePrefillMCPCatalogEntry();

  const [form, setForm] = useState<FormState>(emptyForm);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [secretServerId, setSecretServerId] = useState("");
  const [secretName, setSecretName] = useState("");
  const [secretValue, setSecretValue] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);

  // rows is the common render list, whatever the persistence mode.
  const rows: Row[] =
    scope.kind === "workerVersion"
      ? scope.value.map(inlineRow)
      : listed.map(ownedRow);

  const mechanismBySlug = new Map(catalog.map((c) => [c.slug, c.installMechanism]));
  const runtimeAvailable = (m: string) => {
    if (m === "remote_url") return true;
    return !!runtimes?.[m];
  };
  const mechanismFor = (slug: string) => mechanismBySlug.get(slug) ?? "";

  function writeInline(next: InlineMCP[]) {
    if (scope.kind === "workerVersion") scope.onChange(next);
  }

  async function handleSubmit() {
    setActionError(null);
    const args = form.args.split("\n").map((a) => a.trim()).filter(Boolean);
    try {
      if (scope.kind === "workerVersion") {
        // Inline spec: no row, no RPC — write the version's array.
        const spec: InlineMCP = {
          id: editingId ?? form.name.trim(),
          type: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO ? "stdio" : "http",
          command: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO ? [form.command, ...args].filter(Boolean) : undefined,
          url: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP ? form.url : undefined,
          env: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO ? parseKeyValue(form.env) : undefined,
          headers: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP ? parseKeyValue(form.headers) : undefined,
          enabled: form.enabled,
        };
        const next = editingId
          ? scope.value.map((s) => (s.id === editingId ? { ...spec, id: editingId } : s))
          : [...scope.value, spec];
        writeInline(next);
      } else if (editingId) {
        // The owner ECHO is required on update — a differing echo is rejected.
        await updateServer.mutateAsync({
          id: editingId,
          command: form.command,
          replaceArgs: true,
          args,
          env: parseKeyValue(form.env),
          replaceEnv: true,
          url: form.url,
          headers: parseKeyValue(form.headers),
          replaceHeaders: true,
          enabled: form.enabled,
          projectId: scope.kind === "project" ? scope.projectId : "",
          conversationId: scope.kind === "conversation" ? scope.conversationId : "",
        });
      } else {
        await createServer.mutateAsync({
          name: form.name,
          transport: form.transport,
          command: form.command,
          args,
          env: parseKeyValue(form.env),
          url: form.url,
          headers: parseKeyValue(form.headers),
          enabled: form.enabled,
          projectId: scope.kind === "project" ? scope.projectId : "",
          conversationId: scope.kind === "conversation" ? scope.conversationId : "",
        });
      }
      setForm(emptyForm);
      setEditingId(null);
      setShowForm(false);
    } catch (e) {
      setActionError(String(e));
    }
  }

  function startEdit(r: Row) {
    setEditingId(r.id);
    setForm({
      name: r.name,
      transport: r.transport,
      command: r.command ?? "",
      args: (r.args ?? []).join("\n"),
      // The update replaces env/headers, so the edit form must start from what
      // the entry already holds or saving would silently erase it.
      env: keyValueText(r.env),
      url: r.url ?? "",
      headers: keyValueText(r.headers),
      enabled: r.enabled,
    });
    setShowForm(true);
  }

  async function handleCatalogAdd(slug: string) {
    setActionError(null);
    try {
      const res = await prefill.mutateAsync({ slug });
      const p = res.prefill;
      setForm({
        name: p?.name ?? "",
        transport: p?.transport ?? MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO,
        command: p?.command ?? "",
        args: (p?.args ?? []).join("\n"),
        env: keyValueText(p?.env),
        url: p?.url ?? "",
        headers: keyValueText(p?.headers),
        enabled: p?.enabled ?? true,
      });
      setEditingId(null);
      setShowForm(true);
    } catch (e) {
      setActionError(String(e));
    }
  }

  async function removeRow(id: string) {
    setActionError(null);
    if (scope.kind === "workerVersion") {
      writeInline(scope.value.filter((s) => s.id !== id));
      return;
    }
    try {
      await deleteServer.mutateAsync({ id });
    } catch (e) {
      setActionError(String(e));
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="flex items-center gap-2 text-lg font-semibold">
          <Cable className="h-4 w-4" /> MCP Servers
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Configured servers means THE ENTRIES THIS SCOPE OWNS. Each one
          belongs to exactly this scope — a project, an Ask conversation, or a
          worker version — and is consumed by that scope's resolution union.
          Installations are explicit (click Install); never implicit at session
          time. Credentials persist via the tenant secrets store as write-only{" "}
          {`${'${'}SECRET_NAME}`} references.
        </p>
        <p className="mt-1 text-xs text-muted-foreground">
          A server can no longer be defined once and inherited by several
          projects. The Registry catalog's one-click Add per scope is the
          mitigation.
        </p>
      </div>

      {inheritedFrom && scope.kind !== "project" && (
        <InheritedServers projectId={inheritedFrom.projectId} projectName={inheritedFrom.projectName} />
      )}

      {error && <p className="text-sm text-destructive">Failed to load MCP servers: {String(error)}</p>}
      {actionError && <p className="text-sm text-destructive">Action failed: {actionError}</p>}

      {!readOnly && (
        <div className="flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            onClick={() => {
              setForm(emptyForm);
              setEditingId(null);
              setShowForm(true);
            }}
          >
            <Plus className="mr-1 h-4 w-4" /> Add server
          </Button>
          {runtimes && (
            <span className="text-xs text-muted-foreground">
              runtimes:{" "}
              {Object.entries(runtimes)
                .map(([k, v]) => `${k}${v ? "" : " (missing)"}`)
                .join(", ")}
            </span>
          )}
        </div>
      )}

      {!readOnly && showForm && (
        <Card>
          <CardHeader>
            <CardTitle>{editingId ? "Edit server" : "Add MCP server"}</CardTitle>
            <CardDescription>
              stdio runs a subprocess (command + args + env); streamable
              HTTP connects to a remote endpoint (url + headers).
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="grid gap-3 sm:grid-cols-2">
              <Input
                placeholder="Name (e.g. GitHub)"
                value={form.name}
                disabled={!!editingId}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
              <select
                className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
                value={form.transport}
                onChange={(e) =>
                  setForm({ ...form, transport: Number(e.target.value) })
                }
              >
                <option value={MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO}>stdio</option>
                <option value={MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP}>
                  streamable HTTP
                </option>
              </select>
            </div>
            {form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO ? (
              <>
                <Input
                  placeholder="Command (e.g. npx)"
                  value={form.command}
                  onChange={(e) => setForm({ ...form, command: e.target.value })}
                />
                <Input
                  placeholder="Args (one per line)"
                  value={form.args}
                  onChange={(e) => setForm({ ...form, args: e.target.value })}
                />
                <Input
                  placeholder="Env (KEY=VALUE per line; values may be ${SECRET_NAME})"
                  value={form.env}
                  onChange={(e) => setForm({ ...form, env: e.target.value })}
                />
              </>
            ) : (
              <>
                <Input
                  placeholder="URL (https://…)"
                  value={form.url}
                  onChange={(e) => setForm({ ...form, url: e.target.value })}
                />
                <Input
                  placeholder="Headers (KEY=VALUE per line; values may be ${SECRET_NAME})"
                  value={form.headers}
                  onChange={(e) => setForm({ ...form, headers: e.target.value })}
                />
              </>
            )}
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={form.enabled}
                onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              />
              Enabled
            </label>
            <div className="flex gap-2">
              <Button size="sm" onClick={handleSubmit} disabled={createServer.isPending || updateServer.isPending}>
                {editingId ? "Save" : "Create"}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => { setShowForm(false); setEditingId(null); }}>
                Cancel
              </Button>
            </div>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Configured servers</CardTitle>
          <CardDescription>
            Each entry belongs to exactly one scope. Add the ones you need with
            the Registry catalog below.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {owned && isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {!isLoading && rows.length === 0 && (
            <p className="text-sm text-muted-foreground">No MCP servers configured yet.</p>
          )}
          <div className="space-y-2">
            {rows.map((s) => (
              <div key={s.id} className="flex flex-wrap items-center gap-3 rounded-md border border-white/10 p-3">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{s.name}</span>
                    <span className="text-xs text-muted-foreground">
                      {TRANSPORT_LABEL[s.transport] ?? "?"}
                    </span>
                    {s.catalogSlug && (
                      <span className="text-xs text-muted-foreground">catalog: {s.catalogSlug}</span>
                    )}
                  </div>
                  <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                    <span className={s.enabled ? "text-emerald-500" : ""}>{s.enabled ? "enabled" : "disabled"}</span>
                    {s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO && s.command && (
                      <span className="truncate font-mono">{s.command} {(s.args ?? []).join(" ")}</span>
                    )}
                    {s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP && s.url && (
                      <span className="truncate font-mono">{s.url}</span>
                    )}
                    {owned && <InstallStatusBadge row={s} />}
                    {owned && s.hasSecretStored && (
                      <span className="inline-flex items-center gap-1">
                        <KeyRound className="h-3 w-3" /> secrets stored
                      </span>
                    )}
                  </div>
                </div>
                {!readOnly && (
                  <div className="flex items-center gap-1">
                    {owned && s.catalogSlug && s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO && (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={installServer.isPending || !runtimeAvailable(mechanismFor(s.catalogSlug ?? ""))}
                        title={!runtimeAvailable(mechanismFor(s.catalogSlug ?? "")) ? "runtime missing" : "Run auto-install (explicit)"}
                        onClick={() => installServer.mutateAsync({ id: s.id })}
                      >
                        <Rocket className="mr-1 h-3 w-3" /> Install
                      </Button>
                    )}
                    <Button size="sm" variant="ghost" onClick={() => startEdit(s)}>
                      Edit
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-destructive"
                      onClick={() => removeRow(s.id)}
                    >
                      <Trash2 className="h-3 w-3" />
                    </Button>
                  </div>
                )}
              </div>
            ))}
          </div>
        </CardContent>
      </Card>

      {!readOnly && catalog.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle>Registry catalog</CardTitle>
            <CardDescription>
              Built-in curated MCP servers. One click prefills the add form
              with install specs; required secrets are flagged below.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-2 sm:grid-cols-2">
            {catalog.map((c) => (
              <div key={c.slug} className="flex items-center justify-between gap-2 rounded-md border border-white/10 p-3">
                <div className="min-w-0">
                  <div className="font-medium">{c.displayName}</div>
                  <div className="text-xs text-muted-foreground">
                    {c.installMechanism} · {c.transport}
                  </div>
                  {c.requiredEnv && c.requiredEnv.length > 0 && (
                    <div className="text-xs text-amber-500">
                      secrets: {c.requiredEnv.join(", ")}
                    </div>
                  )}
                </div>
                <Button size="sm" variant="outline" onClick={() => handleCatalogAdd(c.slug)}>
                  <Plus className="mr-1 h-3 w-3" /> Add
                </Button>
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      {/* Credentials are OWNED-MODE ONLY: the secrets store is
       *  tenant-scoped (RLS needs a tenant) and an inline worker spec has no
       *  row to attach a credential to — the operator supplies a
       *  ${SECRET_NAME} in the spec's env/headers instead. */}
      {!readOnly && owned && (
        <Card>
          <CardHeader>
            <CardTitle>Credentials</CardTitle>
            <CardDescription>
              Write credentials for a server's required secrets (e.g.
              GITHUB_TOKEN). Stored in the tenant secrets store (the store is
              tenant-scoped because RLS needs a tenant) — never returned by the
              API, resolved at session time.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2">
            <div className="flex flex-wrap gap-2">
              <select
                className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
                value={secretServerId}
                onChange={(e) => setSecretServerId(e.target.value)}
              >
                <option value="">Server…</option>
                {rows.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
              <Input
                className="max-w-40"
                placeholder="Env var name (e.g. GITHUB_TOKEN)"
                value={secretName}
                onChange={(e) => setSecretName(e.target.value)}
              />
              <Input
                className="max-w-60"
                placeholder="Secret value (write-only)"
                type="password"
                value={secretValue}
                onChange={(e) => setSecretValue(e.target.value)}
              />
              <Button
                size="sm"
                disabled={!secretServerId || !secretName || !secretValue}
                onClick={async () => {
                  try {
                    await setSecret.mutateAsync({ id: secretServerId, name: secretName, value: secretValue });
                    setSecretName("");
                    setSecretValue("");
                  } catch (e) {
                    setActionError(String(e));
                  }
                }}
              >
                <KeyRound className="mr-1 h-3 w-3" /> Save secret
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
