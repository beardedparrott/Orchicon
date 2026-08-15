import { useMemo, useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { Timestamp } from "@bufbuild/protobuf";

import {
  useListTenants,
  useCreateTenant,
  useListIdentities,
  useCreateIdentity,
  useUpdateIdentity,
  useSetIdentityStatus,
  useDeleteIdentity,
  useListRoles,
  useListApiKeys,
  useListAuditEntries,
  useListAuditEvents,
  type AuditEventFilters,
  useCreateRole,
  useAssignRole,
  useCreateApiKey,
  useRevokeApiKey,
  useRotateApiKey,
  useSetLocalCredential,
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
  const create = useCreateIdentity();
  const update = useUpdateIdentity();
  const setStatus = useSetIdentityStatus();
  const remove = useDeleteIdentity();
  const setCredential = useSetLocalCredential();
  const toast = useToast();

  // Create form state.
  const [createType, setCreateType] = useState<"user" | "service">("user");
  const [createSubject, setCreateSubject] = useState("");
  const [createName, setCreateName] = useState("");

  // "Set local password" state (unchanged surface).
  const [identityId, setIdentityId] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  // List-page pattern: search + status filter + selection.
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // Inline row edit.
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return (data ?? []).filter((i) => {
      if (statusFilter !== "all" && i.status !== statusFilter) return false;
      if (!q) return true;
      return (
        (i.displayName || "").toLowerCase().includes(q) ||
        i.subject.toLowerCase().includes(q)
      );
    });
  }, [data, search, statusFilter]);

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const toggleSelectAll = () => {
    setSelected((prev) =>
      prev.size === filtered.length && filtered.length > 0
        ? new Set()
        : new Set(filtered.map((i) => i.id))
    );
  };

  async function handleCreate() {
    const name = createName.trim();
    const subject = createSubject.trim();
    if (!name) return;
    if (createType === "user" && !subject) return;
    try {
      const i = await create.mutateAsync({
        identityType: createType,
        subject: subject || undefined,
        displayName: name,
      });
      toast.success(`Identity "${i?.displayName ?? name}" created.`);
      setCreateName("");
      setCreateSubject("");
    } catch {
      // global onError already toasted the error
    }
  }

  function handleEditStart(id: string) {
    const i = data?.find((x) => x.id === id);
    setEditingId(id);
    setEditName(i?.displayName ?? "");
  }

  async function handleEditSave(id: string) {
    const name = editName.trim();
    if (!name) return;
    try {
      await update.mutateAsync({ id, displayName: name });
      toast.success("Identity updated.");
      setEditingId(null);
      setEditName("");
    } catch {
      // error already toasted
    }
  }

  async function handleToggleStatus(id: string, subject: string, current: string) {
    const next = current === "disabled" ? "active" : "disabled";
    try {
      await setStatus.mutateAsync({ id, status: next });
      toast.success(`Identity "${subject}" ${next === "disabled" ? "disabled" : "enabled"}.`);
      setSelected((prev) => {
        const s = new Set(prev);
        s.delete(id);
        return s;
      });
    } catch {
      // error already toasted
    }
  }

  async function handleDelete(id: string, subject: string) {
    if (!window.confirm(`Delete identity "${subject}"? This removes its role bindings, API keys, and local credential and cannot be undone.`)) return;
    try {
      await remove.mutateAsync(id);
      toast.success(`Identity "${subject}" deleted.`);
      setSelected((prev) => {
        const s = new Set(prev);
        s.delete(id);
        return s;
      });
    } catch {
      // error already toasted
    }
  }

  async function handleBulkDisable() {
    const targets = filtered.filter((i) => selected.has(i.id) && i.status === "active");
    if (targets.length === 0) {
      toast.error("No active identities selected to disable.");
      return;
    }
    if (!window.confirm(`Disable ${targets.length} selected active identit${targets.length === 1 ? "y" : "ies"}?`)) return;
    try {
      await Promise.all(targets.map((i) => setStatus.mutateAsync({ id: i.id, status: "disabled" })));
      toast.success(`${targets.length} identity${targets.length === 1 ? "" : "ies"} disabled.`);
      setSelected(new Set());
    } catch {
      // error already toasted
    }
  }

  async function handleBulkDelete() {
    const targets = filtered.filter((i) => selected.has(i.id) && i.status === "disabled");
    const blocked = filtered.filter((i) => selected.has(i.id) && i.status !== "disabled");
    if (targets.length === 0) {
      toast.error(blocked.length > 0 ? "Only disabled identities can be deleted; disable them first." : "No disabled identities selected.");
      return;
    }
    if (!window.confirm(`Delete ${targets.length} selected disabled identit${targets.length === 1 ? "y" : "ies"}? This cannot be undone.`)) return;
    try {
      await Promise.all(targets.map((i) => remove.mutateAsync(i.id)));
      toast.success(`${targets.length} identity${targets.length === 1 ? "" : "ies"} deleted.`);
      if (blocked.length > 0) {
        toast.error(`${blocked.length} active identit${blocked.length === 1 ? "y" : "ies"} skipped — disable them before deleting.`);
      }
      setSelected(new Set());
    } catch {
      // error already toasted
    }
  }

  async function handleSetCredential() {
    if (!identityId || !username || !password) return;
    try {
      await setCredential.mutateAsync({ identityId, username, password });
      toast.success(`Local credential set for ${username}.`);
      setPassword("");
    } catch {
      /* error already toasted by global handler */
    }
  }

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading…</p>;
  if (error) return <p className="text-sm text-destructive">{String(error)}</p>;
  return (
    <div className="space-y-6">
      {/* Create form */}
      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Create identity</h3>
        <div className="grid gap-2 md:grid-cols-[auto_1fr_1fr_auto]">
          <select
            value={createType}
            onChange={(e) => setCreateType(e.target.value as "user" | "service")}
            className="rounded-md border border-input bg-background px-3 py-2 text-sm"
            aria-label="Identity type"
          >
            <option value="user">User</option>
            <option value="service">Service account</option>
          </select>
          <Input
            placeholder={createType === "user" ? "subject (login handle)" : "subject (slug, optional)"}
            value={createSubject}
            onChange={(e) => setCreateSubject(e.target.value)}
            disabled={create.isPending}
          />
          <Input
            placeholder="Display name"
            value={createName}
            onChange={(e) => setCreateName(e.target.value)}
            disabled={create.isPending}
          />
          <Button
            onClick={handleCreate}
            disabled={!createName.trim() || (createType === "user" && !createSubject.trim()) || create.isPending}
          >
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </div>
        <p className="text-xs text-muted-foreground">
          {createType === "user"
            ? "Subject is the login handle and must match "
            : "Subject is optional; when omitted a synthetic sa-… subject is generated. Must match "}
          <code className="font-mono">{createType === "user" ? "^[a-z0-9][a-z0-9._@+-]*$" : "^[a-z0-9]+(?:-[a-z0-9]+)*$"}</code>.
        </p>
      </div>

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-3">
        <Input
          placeholder="Search identities…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          className="h-9 rounded-md border bg-background px-2 text-sm"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          aria-label="Status filter"
        >
          <option value="all">All statuses</option>
          <option value="active">Active</option>
          <option value="disabled">Disabled</option>
        </select>
        {selected.size > 0 && (
          <>
            <Button size="sm" variant="outline" onClick={handleBulkDisable} disabled={setStatus.isPending}>
              Disable {selected.size} selected
            </Button>
            <Button size="sm" variant="destructive" onClick={handleBulkDelete} disabled={remove.isPending}>
              Delete {selected.size} selected
            </Button>
          </>
        )}
      </div>

      {/* Select-all header */}
      {filtered.length > 0 && (
        <div className="flex items-center gap-2 px-1">
          <input
            type="checkbox"
            checked={filtered.length > 0 && selected.size === filtered.length}
            onChange={toggleSelectAll}
            className="h-4 w-4 rounded border-input"
            aria-label="Select all identities"
          />
          <span className="text-xs text-muted-foreground">
            {selected.size > 0
              ? `${selected.size} of ${filtered.length} selected`
              : `${filtered.length} identit${filtered.length === 1 ? "y" : "ies"}`}
          </span>
        </div>
      )}

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4" />
              <th className="py-2 pr-4">ID</th>
              <th className="py-2 pr-4">Subject</th>
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Type</th>
              <th className="py-2 pr-4">Status</th>
              <th className="py-2 pr-4">Actions</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 && (
              <tr>
                <td colSpan={7} className="py-3 text-muted-foreground">
                  No identities match.
                </td>
              </tr>
            )}
            {filtered.map((i) => (
              <tr key={i.id} className="border-b">
                <td className="py-2 pr-4">
                  <input
                    type="checkbox"
                    checked={selected.has(i.id)}
                    onChange={() => toggleSelect(i.id)}
                    className="h-4 w-4 rounded border-input"
                    aria-label={`Select ${i.subject}`}
                  />
                </td>
                <td className="py-2 pr-4 font-mono text-xs">{i.id}</td>
                <td className="py-2 pr-4 font-mono text-xs">{i.subject}</td>
                <td className="py-2 pr-4">
                  {editingId === i.id ? (
                    <div className="flex items-center gap-2">
                      <Input
                        className="h-8 w-56"
                        value={editName}
                        onChange={(e) => setEditName(e.target.value)}
                        autoFocus
                      />
                      <Button size="sm" onClick={() => handleEditSave(i.id)} disabled={update.isPending || !editName.trim()}>
                        Save
                      </Button>
                      <Button size="sm" variant="outline" onClick={() => { setEditingId(null); setEditName(""); }}>
                        Cancel
                      </Button>
                    </div>
                  ) : (
                    i.displayName
                  )}
                </td>
                <td className="py-2 pr-4">{i.identityType}</td>
                <td className="py-2 pr-4">
                  <span className={cn(
                    "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium",
                    i.status === "active" ? "bg-green-100 text-green-800 dark:bg-green-950/50 dark:text-green-300"
                      : "bg-muted text-muted-foreground"
                  )}>
                    {i.status}
                  </span>
                </td>
                <td className="py-2 pr-4 space-x-2 whitespace-nowrap">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => handleEditStart(i.id)}
                    disabled={editingId === i.id}
                  >
                    Edit
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => handleToggleStatus(i.id, i.subject, i.status)}
                    disabled={setStatus.isPending || editingId === i.id}
                  >
                    {i.status === "disabled" ? "Enable" : "Disable"}
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    onClick={() => handleDelete(i.id, i.subject)}
                    disabled={remove.isPending || i.status !== "disabled"}
                    title={i.status === "disabled" ? "Delete identity" : "Disable the identity before deleting"}
                  >
                    Delete
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="space-y-2 border-t pt-4">
        <h3 className="text-sm font-semibold">Set local password</h3>
        <p className="text-xs text-muted-foreground">
          Create or change the embedded-IdP login credential for an identity
          (admin-only, auth:write). Use this to change the default admin's
          password after first boot.
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <select
            value={identityId}
            onChange={(e) => setIdentityId(e.target.value)}
            className="rounded-md border border-input bg-background px-3 py-2 text-sm"
            aria-label="Identity"
          >
            <option value="">Select identity…</option>
            {data?.map((i) => (
              <option key={i.id} value={i.id}>
                {i.subject}
              </option>
            ))}
          </select>
          <Input
            className="w-40"
            placeholder="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
          <Input
            className="w-56"
            type="password"
            placeholder="new password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          <Button
            onClick={handleSetCredential}
            disabled={!identityId || !username || !password || setCredential.isPending}
          >
            {setCredential.isPending ? "Setting…" : "Set password"}
          </Button>
        </div>
      </div>
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
  const [view, setView] = useState<"decisions" | "events">("events");
  return (
    <div className="space-y-4">
      <div className="flex gap-1 border-b">
        {(["events", "decisions"] as const).map((v) => (
          <button
            key={v}
            onClick={() => setView(v)}
            className={cn(
              "px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors",
              view === v
                ? "border-primary text-primary"
                : "border-transparent text-muted-foreground hover:text-foreground"
            )}
          >
            {v[0].toUpperCase() + v.slice(1)}
          </button>
        ))}
      </div>
      {view === "decisions" && <AuditDecisionsView />}
      {view === "events" && <AuditEventsView />}
    </div>
  );
}

// AuditDecisionsView is the existing policy-decision trail (ListAuditEntries).
function AuditDecisionsView() {
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

// AuditEventsView is the actor-based audit_events trail (ListAuditEvents):
// who did what, how they authenticated, and the before/after snapshot. A
// filter bar (actor/action/target/time) scopes the query; time inputs are
// browser-local datetime-local values converted to UTC timestamps before
// they reach the RPC (occurred_at is stored/compared in UTC).
function AuditEventsView() {
  const [draft, setDraft] = useState({
    action: "",
    actorId: "",
    targetType: "",
    targetId: "",
    from: "",
    to: "",
  });
  const [applied, setApplied] = useState<AuditEventFilters>({});
  const { data, isLoading } = useListAuditEvents(applied);

  const set = (k: keyof typeof draft) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setDraft((d) => ({ ...d, [k]: e.target.value }));

  const apply = () => {
    const next: AuditEventFilters = {
      action: draft.action || undefined,
      actorId: draft.actorId || undefined,
      targetType: draft.targetType || undefined,
      targetId: draft.targetId || undefined,
      startTime: draft.from
        ? Timestamp.fromDate(new Date(draft.from))
        : undefined,
      endTime: draft.to ? Timestamp.fromDate(new Date(draft.to)) : undefined,
    };
    setApplied(next);
  };

  const clear = () => {
    setDraft({ action: "", actorId: "", targetType: "", targetId: "", from: "", to: "" });
    setApplied({});
  };

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading…</p>;
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <div>
          <Label htmlFor="audit-action">Action</Label>
          <Input
            id="audit-action"
            className="mt-1 h-9 w-44 font-mono text-xs"
            placeholder="e.g. work_item.created"
            value={draft.action}
            onChange={set("action")}
          />
        </div>
        <div>
          <Label htmlFor="audit-actor">Actor</Label>
          <Input
            id="audit-actor"
            className="mt-1 h-9 w-44 font-mono text-xs"
            placeholder="identity id"
            value={draft.actorId}
            onChange={set("actorId")}
          />
        </div>
        <div>
          <Label htmlFor="audit-target-type">Target type</Label>
          <Input
            id="audit-target-type"
            className="mt-1 h-9 w-36 font-mono text-xs"
            placeholder="e.g. work_item"
            value={draft.targetType}
            onChange={set("targetType")}
          />
        </div>
        <div>
          <Label htmlFor="audit-target-id">Target ID</Label>
          <Input
            id="audit-target-id"
            className="mt-1 h-9 w-44 font-mono text-xs"
            placeholder="entity id"
            value={draft.targetId}
            onChange={set("targetId")}
          />
        </div>
        <div>
          <Label htmlFor="audit-from">From (local time)</Label>
          <input
            id="audit-from"
            type="datetime-local"
            value={draft.from}
            onChange={set("from")}
            className="mt-1 h-9 w-52 rounded-md border bg-background px-2 text-sm"
          />
        </div>
        <div>
          <Label htmlFor="audit-to">To (local time)</Label>
          <input
            id="audit-to"
            type="datetime-local"
            value={draft.to}
            onChange={set("to")}
            className="mt-1 h-9 w-52 rounded-md border bg-background px-2 text-sm"
          />
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={apply}>
            Apply filters
          </Button>
          <Button size="sm" variant="outline" onClick={clear}>
            Clear
          </Button>
        </div>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4">Action</th>
              <th className="py-2 pr-4">Actor</th>
              <th className="py-2 pr-4">Auth</th>
              <th className="py-2 pr-4">Target</th>
              <th className="py-2 pr-4">Before/After</th>
              <th className="py-2 pr-4">Trace</th>
              <th className="py-2 pr-4">When</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((e) => (
              <tr key={e.id} className="border-b">
                <td className="py-2 pr-4 font-mono text-xs">{e.action}</td>
                <td className="py-2 pr-4 font-mono text-xs">
                  {e.actorIdentityId || "-"}
                </td>
                <td className="py-2 pr-4 text-xs">{e.authMethod || "-"}</td>
                <td className="py-2 pr-4 font-mono text-xs">
                  {e.targetType}:{e.targetId}
                </td>
                <td className="py-2 pr-4 text-xs">
                  <BeforeAfterCell before={e.before} after={e.after} />
                </td>
                <td className="py-2 pr-4 font-mono text-xs">{e.traceId}</td>
                <td className="py-2 pr-4 text-xs">
                  {e.occurredAt?.toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function BeforeAfterCell({ before, after }: { before?: string; after?: string }) {
  const render = (v?: string) => {
    if (!v || v === "{}" || v === "") return null;
    let body = v;
    try {
      body = JSON.stringify(JSON.parse(v));
    } catch {
      // leave as-is for non-JSON snapshots
    }
    return body;
  };
  const beforeBody = render(before);
  const afterBody = render(after);
  if (!beforeBody && !afterBody) return <span>-</span>;
  return (
    <span className="whitespace-pre-wrap font-mono text-[11px]">
      {beforeBody ? `→ ${beforeBody}` : ""}
      {afterBody ? `${beforeBody ? " " : ""}→ ${afterBody}` : ""}
    </span>
  );
}
