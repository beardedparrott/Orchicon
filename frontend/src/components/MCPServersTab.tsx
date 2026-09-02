// MCP tab (ADR-0008): tenant MCP server management under
// Settings → Adapters → MCP. Full CRUD over tenant-scoped MCP server
// entries (stdio + streamable HTTP), the curated registry catalog with
// one-click prefill, explicit-only auto-install (dry-run in CI), and
// write-only credentials via the tenant secrets store (provider-token
// pattern — never baked, never returned). Selections are references.
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
  useGetTenantDefaultMCPServers,
  useSetTenantDefaultMCPServers,
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

function InstallStatusBadge({ server }: { server: { installStatus: number; installResult?: { ok?: boolean; error?: string } | null } }) {
  const s = server.installStatus;
  if (s === 4) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-emerald-500">
        <CheckCircle2 className="h-3 w-3" /> installed
      </span>
    );
  }
  if (s === 5) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-destructive" title={server.installResult?.error ?? ""}>
        <XCircle className="h-3 w-3" /> failed
      </span>
    );
  }
  if (s === 3) {
    return <span className="text-xs text-muted-foreground">installing…</span>;
  }
  return <span className="text-xs text-muted-foreground">not installed</span>;
}

export function MCPServersTab() {
  const { data: servers = [], isLoading, error } = useMCPServerList();
  const { data: catalog = [] } = useMCPCatalog();
  const { data: runtimes } = useMCPRuntimes();
  const { data: defaultIds = [] } = useGetTenantDefaultMCPServers();

  const createServer = useCreateMCPServer();
  const updateServer = useUpdateMCPServer();
  const deleteServer = useDeleteMCPServer();
  const installServer = useInstallMCPServer();
  const setSecret = useSetMCPServerSecret();
  const prefill = usePrefillMCPCatalogEntry();
  const setDefault = useSetTenantDefaultMCPServers();

  const [form, setForm] = useState<FormState>(emptyForm);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [secretServerId, setSecretServerId] = useState("");
  const [secretName, setSecretName] = useState("");
  const [secretValue, setSecretValue] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);

  const mechanismBySlug = new Map(catalog.map((c) => [c.slug, c.installMechanism]));
  const runtimeAvailable = (m: string) => {
    if (m === "remote_url") return true;
    return !!runtimes?.[m];
  };
  const mechanismFor = (slug: string) => mechanismBySlug.get(slug) ?? "";

  async function handleSubmit() {
    setActionError(null);
    try {
      if (editingId) {
        await updateServer.mutateAsync({
          id: editingId,
          command: form.command,
          replaceArgs: true,
          args: form.args.split("\n").map((a) => a.trim()).filter(Boolean),
          env: parseKeyValue(form.env),
          replaceEnv: true,
          url: form.url,
          headers: parseKeyValue(form.headers),
          replaceHeaders: true,
          enabled: form.enabled,
        });
      } else {
        await createServer.mutateAsync({
          name: form.name,
          transport: form.transport,
          command: form.command,
          args: form.args.split("\n").map((a) => a.trim()).filter(Boolean),
          env: parseKeyValue(form.env),
          url: form.url,
          headers: parseKeyValue(form.headers),
          enabled: form.enabled,
        });
      }
      setForm(emptyForm);
      setEditingId(null);
      setShowForm(false);
    } catch (e) {
      setActionError(String(e));
    }
  }

  function startEdit(s: { id: string; name: string; transport: number; command?: string; args?: string[]; env?: Record<string, string>; url?: string; headers?: Record<string, string>; enabled: boolean }) {
    setEditingId(s.id);
    setForm({
      name: s.name,
      transport: s.transport,
      command: s.command ?? "",
      args: (s.args ?? []).join("\n"),
      env: keyValueText(s.env),
      url: s.url ?? "",
      headers: keyValueText(s.headers),
      enabled: s.enabled,
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

  async function toggleDefault(id: string) {
    const next = defaultIds.includes(id)
      ? defaultIds.filter((x) => x !== id)
      : [...defaultIds, id];
    await setDefault.mutateAsync({ mcpServerIds: next });
  }

  return (
    <div className="space-y-6">
      <div>
        <h2 className="flex items-center gap-2 text-lg font-semibold">
          <Cable className="h-4 w-4" /> MCP Servers
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Tenant-scoped MCP server entries. Projects and workers reference
          these by id — editing one entry updates every consumer.
          Installations are explicit (click Install); never implicit at
          session time. Credentials persist via the tenant secrets store
          as write-only {`${'${'}SECRET_NAME}`} references.
        </p>
      </div>

      {error && <p className="text-sm text-destructive">Failed to load MCP servers: {String(error)}</p>}
      {actionError && <p className="text-sm text-destructive">Action failed: {actionError}</p>}

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

      {showForm && (
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
            Check a server to make it the tenant default (used when a
            worker and project specify none). Selections are references,
            never copies.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {!isLoading && servers.length === 0 && (
            <p className="text-sm text-muted-foreground">No MCP servers configured yet.</p>
          )}
          <div className="space-y-2">
            {servers.map((s) => (
              <div key={s.id} className="flex flex-wrap items-center gap-3 rounded-md border border-white/10 p-3">
                <input
                  type="checkbox"
                  title="Tenant default"
                  checked={defaultIds.includes(s.id)}
                  onChange={() => toggleDefault(s.id)}
                />
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
                    <InstallStatusBadge server={s} />
                    {s.hasSecretStored && (
                      <span className="inline-flex items-center gap-1">
                        <KeyRound className="h-3 w-3" /> secrets stored
                      </span>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-1">
                  {s.catalogSlug && s.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO && (
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
                    onClick={async () => {
                      try {
                        await deleteServer.mutateAsync({ id: s.id });
                      } catch (e) {
                        setActionError(String(e));
                      }
                    }}
                  >
                    <Trash2 className="h-3 w-3" />
                  </Button>
                </div>
              </div>
            ))}
          </div>
        </CardContent>
      </Card>

      {catalog.length > 0 && (
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

      <Card>
        <CardHeader>
          <CardTitle>Credentials</CardTitle>
          <CardDescription>
            Write credentials for a server's required secrets (e.g.
            GITHUB_TOKEN). Stored in the tenant secrets store — never
            returned by the API, resolved at session time.
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
              {servers.map((s) => (
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
    </div>
  );
}
