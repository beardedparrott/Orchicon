import { useState } from "react";
import { createRoute } from "@tanstack/react-router";

import {
  useListTenants,
  useCreateTenant,
  useListIdentities,
  useListRoles,
  useListApiKeys,
  useListAuditEntries,
  useCreateRole,
  useAssignRole,
  useCreateApiKey,
  useRevokeApiKey,
  useRotateApiKey,
} from "@/api/auth";
import { useToast } from "@/components/ui/toast";
import { useIsAdmin } from "@/auth/auth";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Admin surface (docs/10 §5): tenants, identities, roles, API keys, audit.
// RBAC-gated: only tenant admins see this route's content. The server
// still enforces auth:write on every mutating RPC (docs/10 §10 #5).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/admin",
  component: AdminPage,
});

type Tab = "tenants" | "identities" | "roles" | "apikeys" | "audit";

function AdminPage() {
  const isAdmin = useIsAdmin();
  const [tab, setTab] = useState<Tab>("tenants");
  if (!isAdmin) {
    return (
      <div className="space-y-6">
        <h1 className="text-2xl font-semibold tracking-tight">Admin</h1>
        <p className="text-sm text-muted-foreground">
          You do not have admin privileges in this tenant.
        </p>
      </div>
    );
  }
  return (
    <div className="space-y-6">
      <h1 className="text-2xl font-semibold tracking-tight">Admin</h1>
      <div className="flex gap-1 border-b">
        {(["tenants", "identities", "roles", "apikeys", "audit"] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={cn(
              "px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors",
              tab === t
                ? "border-primary text-primary"
                : "border-transparent text-muted-foreground hover:text-foreground"
            )}
          >
            {t === "apikeys" ? "API Keys" : t[0].toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>
      {tab === "tenants" && <TenantsTab />}
      {tab === "identities" && <IdentitiesTab />}
      {tab === "roles" && <RolesTab />}
      {tab === "apikeys" && <ApiKeysTab />}
      {tab === "audit" && <AuditTab />}
    </div>
  );
}

function TenantsTab() {
  const { data, isLoading, error } = useListTenants();
  const create = useCreateTenant();
  const toast = useToast();
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");

  async function handleCreate() {
    const trimmedSlug = slug.trim();
    const trimmedName = name.trim();
    if (!trimmedSlug || !trimmedName) return;
    try {
      const t = await create.mutateAsync({
        slug: trimmedSlug,
        name: trimmedName,
      });
      toast.success(`Tenant "${t?.name ?? trimmedName}" created.`, {
        title: `Slug: ${t?.slug ?? trimmedSlug}`,
      });
      setSlug("");
      setName("");
    } catch {
      // global onError already toasted the error
    }
  }

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Create tenant</h3>
        <div className="grid gap-2 md:grid-cols-[200px_1fr_auto]">
          <Input
            placeholder="acme"
            value={slug}
            onChange={(e) => setSlug(e.target.value)}
            disabled={create.isPending}
          />
          <Input
            placeholder="Acme Corporation"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={create.isPending}
          />
          <Button
            onClick={handleCreate}
            disabled={!slug.trim() || !name.trim() || create.isPending}
          >
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          Slug must match <code className="font-mono">^[a-z0-9]+(?:-[a-z0-9]+)*$</code> and
          is used as the unique identifier (e.g. <code className="font-mono">acme</code>).
        </p>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4">ID</th>
              <th className="py-2 pr-4">Slug</th>
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Status</th>
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr>
                <td colSpan={4} className="py-3 text-muted-foreground">
                  Loading…
                </td>
              </tr>
            )}
            {error && !isLoading && (
              <tr>
                <td colSpan={4} className="py-3 text-destructive">
                  Failed to load: {String(error)}
                </td>
              </tr>
            )}
            {data && data.length === 0 && !isLoading && (
              <tr>
                <td colSpan={4} className="py-3 text-muted-foreground">
                  No tenants yet.
                </td>
              </tr>
            )}
            {data?.map((t) => (
              <tr key={t.id} className="border-b">
                <td className="py-2 pr-4 font-mono text-xs">{t.id}</td>
                <td className="py-2 pr-4 font-mono text-xs">{t.slug}</td>
                <td className="py-2 pr-4">{t.name}</td>
                <td className="py-2 pr-4">{t.status}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function IdentitiesTab() {
  const { data, isLoading, error } = useListIdentities();
  if (isLoading) return <p className="text-sm text-muted-foreground">Loading…</p>;
  if (error) return <p className="text-sm text-destructive">{String(error)}</p>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="py-2 pr-4">ID</th>
            <th className="py-2 pr-4">Subject</th>
            <th className="py-2 pr-4">Name</th>
            <th className="py-2 pr-4">Type</th>
            <th className="py-2 pr-4">Status</th>
          </tr>
        </thead>
        <tbody>
          {data?.map((i) => (
            <tr key={i.id} className="border-b">
              <td className="py-2 pr-4 font-mono text-xs">{i.id}</td>
              <td className="py-2 pr-4 font-mono text-xs">{i.subject}</td>
              <td className="py-2 pr-4">{i.displayName}</td>
              <td className="py-2 pr-4">{i.identityType}</td>
              <td className="py-2 pr-4">{i.status}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RolesTab() {
  const { data } = useListRoles();
  const createRole = useCreateRole();
  const assignRole = useAssignRole();
  const toast = useToast();
  const [name, setName] = useState("");
  const [ents, setEnts] = useState("project:create,project:write");
  const [identityId, setIdentityId] = useState("");
  const [roleId, setRoleId] = useState("");

  async function handleCreateRole() {
    try {
      const r = await createRole.mutateAsync({
        name,
        scope: "tenant",
        entitlements: ents.split(",").map((s) => s.trim()).filter(Boolean),
      });
      toast.success(`Role "${r?.name ?? name}" created.`);
      setName("");
    } catch {
      /* error already toasted by global handler */
    }
  }

  async function handleAssignRole() {
    if (!identityId || !roleId) return;
    try {
      await assignRole.mutateAsync({ identityId, roleId, scope: "tenant" });
      toast.success("Role assigned.");
      setIdentityId("");
      setRoleId("");
    } catch {
      /* error already toasted */
    }
  }

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Create role</h3>
        <div className="flex gap-2">
          <Input placeholder="role-name" value={name} onChange={(e) => setName(e.target.value)} />
          <Input
            className="flex-1"
            placeholder="entitlements (comma-separated)"
            value={ents}
            onChange={(e) => setEnts(e.target.value)}
          />
          <Button
            onClick={handleCreateRole}
            disabled={!name || createRole.isPending}
          >
            {createRole.isPending ? "Creating…" : "Create"}
          </Button>
        </div>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4">ID</th>
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Scope</th>
              <th className="py-2 pr-4">Entitlements</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((r) => (
              <tr key={r.id} className="border-b">
                <td className="py-2 pr-4 font-mono text-xs">{r.id}</td>
                <td className="py-2 pr-4">{r.name}</td>
                <td className="py-2 pr-4">{r.scope}</td>
                <td className="py-2 pr-4 font-mono text-xs">
                  {(r.entitlements ?? []).join(", ")}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Assign role</h3>
        <div className="flex gap-2">
          <Input placeholder="identity id" value={identityId} onChange={(e) => setIdentityId(e.target.value)} />
          <Input placeholder="role id" value={roleId} onChange={(e) => setRoleId(e.target.value)} />
          <Button
            onClick={handleAssignRole}
            disabled={!identityId || !roleId || assignRole.isPending}
          >
            {assignRole.isPending ? "Assigning…" : "Assign"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function ApiKeysTab() {
  const { data } = useListApiKeys();
  const create = useCreateApiKey();
  const revoke = useRevokeApiKey();
  const rotate = useRotateApiKey();
  const toast = useToast();
  const [identityId, setIdentityId] = useState("");
  const [keyName, setKeyName] = useState("");
  const [scopes, setScopes] = useState("project:read,project:write");
  const [secret, setSecret] = useState("");

  async function handleCreate() {
    if (!identityId || !keyName) return;
    try {
      const res = await create.mutateAsync({
        identityId,
        name: keyName,
        scopes: scopes.split(",").map((s) => s.trim()).filter(Boolean),
      });
      toast.success(`API key "${keyName}" created.`);
      setSecret(res.secret?.key ?? "");
      setKeyName("");
    } catch {
      /* error already toasted */
    }
  }

  async function handleRotate(id: string, name: string) {
    try {
      const res = await rotate.mutateAsync(id);
      toast.success(`Key "${name}" rotated.`);
      if (res?.secret?.key) {
        setSecret(res.secret.key);
      }
    } catch {
      /* error already toasted */
    }
  }

  async function handleRevoke(id: string, name: string) {
    try {
      await revoke.mutateAsync(id);
      toast.success(`Key "${name}" revoked.`);
    } catch {
      /* error already toasted */
    }
  }

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Create API key</h3>
        <div className="grid gap-2 md:grid-cols-3">
          <Input placeholder="identity id" value={identityId} onChange={(e) => setIdentityId(e.target.value)} />
          <Input placeholder="key name" value={keyName} onChange={(e) => setKeyName(e.target.value)} />
          <Input placeholder="scopes" value={scopes} onChange={(e) => setScopes(e.target.value)} />
        </div>
        <Button
          onClick={handleCreate}
          disabled={!identityId || !keyName || create.isPending}
        >
          {create.isPending ? "Creating…" : "Create"}
        </Button>
        {secret && (
          <div className="rounded-md bg-yellow-50 border border-yellow-200 p-3 text-xs">
            <Label className="font-semibold text-yellow-900">
              Copy the key now — it will not be shown again.
            </Label>
            <pre className="mt-1 break-all font-mono text-yellow-900">{secret}</pre>
            <Button variant="outline" size="sm" className="mt-2" onClick={() => setSecret("")}>
              Dismiss
            </Button>
          </div>
        )}
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4">Prefix</th>
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Status</th>
              <th className="py-2 pr-4">Scopes</th>
              <th className="py-2 pr-4">Actions</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((k) => (
              <tr key={k.id} className="border-b">
                <td className="py-2 pr-4 font-mono text-xs">{k.prefix}…</td>
                <td className="py-2 pr-4">{k.name}</td>
                <td className="py-2 pr-4">{k.status}</td>
                <td className="py-2 pr-4 font-mono text-xs">{(k.scopes ?? []).join(", ")}</td>
                <td className="py-2 pr-4 space-x-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => handleRotate(k.id, k.name)}
                    disabled={rotate.isPending}
                  >
                    Rotate
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => handleRevoke(k.id, k.name)}
                    disabled={revoke.isPending || k.status === "revoked"}
                  >
                    Revoke
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function AuditTab() {
  const { data, isLoading } = useListAuditEntries();
  if (isLoading) return <p className="text-sm text-muted-foreground">Loading…</p>;
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="py-2 pr-4">Decision Point</th>
            <th className="py-2 pr-4">Effect</th>
            <th className="py-2 pr-4">Actor</th>
            <th className="py-2 pr-4">Target</th>
            <th className="py-2 pr-4">Trace</th>
            <th className="py-2 pr-4">When</th>
          </tr>
        </thead>
        <tbody>
          {data?.map((e) => (
            <tr key={e.id} className="border-b">
              <td className="py-2 pr-4">{e.decisionPoint}</td>
              <td className="py-2 pr-4">{e.effect}</td>
              <td className="py-2 pr-4 font-mono text-xs">{e.actorId}</td>
              <td className="py-2 pr-4 font-mono text-xs">{e.targetId}</td>
              <td className="py-2 pr-4 font-mono text-xs">{e.traceId}</td>
              <td className="py-2 pr-4 text-xs">
                {e.occurredAt?.toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
