import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  useListRoleBindings,
  useListApiKeys,
  useListAuditEntries,
  useListAuditEvents,
  type AuditEventFilters,
  useCreateRole,
  useUpdateRole,
  useDeleteRole,
  useAssignRole,
  useRevokeRole,
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
  const { data: roleBindings } = useListRoleBindings();
  const { data: roles } = useListRoles();
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

  // List-page pattern: search + status filter + selection.
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // Row-edit modal (display name + local credential).
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editUsername, setEditUsername] = useState("");
  const [editPassword, setEditPassword] = useState("");
  const editDialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = editDialogRef.current;
    if (!dialog) return;
    if (editingId) {
      dialog.showModal();
    } else {
      dialog.close();
    }
  }, [editingId]);

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

  // Map identityId -> list of bound role names, resolved from the role
  // table by id (human-readable role names, not ids).
  const roleNameById = useMemo(() => {
    const m = new Map<string, string>();
    for (const r of roles ?? []) m.set(r.id, r.name);
    return m;
  }, [roles]);

  const rolesByIdentity = useMemo(() => {
    const m = new Map<string, string[]>();
    for (const b of roleBindings ?? []) {
      const name = roleNameById.get(b.roleId) ?? b.roleId;
      const cur = m.get(b.identityId) ?? [];
      cur.push(name);
      m.set(b.identityId, cur);
    }
    return m;
  }, [roleBindings, roleNameById]);

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
    // Username prefilled from the identity's local credential (empty when
    // the identity has none — the admin fills it to provision one). Subject
    // != credential username today: the credential's username is the login
    // handle, not the OIDC/SSO subject.
    setEditUsername(i?.username ?? "");
    setEditPassword("");
  }

  async function handleEditSave(id: string) {
    const i = data?.find((x) => x.id === id);
    const name = editName.trim();
    if (!name) return;
    try {
      // Display-name edit (existing UpdateIdentity) — only when changed.
      if (name !== (i?.displayName ?? "")) {
        await update.mutateAsync({ id, displayName: name });
      }
      // Local credential: set only when BOTH username and password are
      // non-empty (username is prefilled for credentialed identities; the
      // admin fills it to provision a new one). Wired to SetLocalCredential
      // (auth:write, admin-only).
      if (editUsername.trim() && editPassword) {
        await setCredential.mutateAsync({
          identityId: id,
          username: editUsername.trim(),
          password: editPassword,
        });
      }
      toast.success("Identity updated.");
      handleEditCancel();
    } catch {
      // error already toasted
    }
  }

  function handleEditCancel() {
    setEditingId(null);
    setEditName("");
    setEditUsername("");
    setEditPassword("");
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
            className="rounded-xl glass-input px-3 py-2 text-sm"
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
      <div className="flex flex-wrap items-center gap-3 rounded-2xl glass-panel p-3 border border-white/10">
        <Input
          placeholder="Search identities…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          className="h-9 rounded-xl glass-input px-3 text-sm"
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
              <th className="py-2 pr-4">Username</th>
              <th className="py-2 pr-4">Type</th>
              <th className="py-2 pr-4">Roles</th>
              <th className="py-2 pr-4">Status</th>
              <th className="py-2 pr-4">Actions</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 && (
              <tr>
                <td colSpan={9} className="py-3 text-muted-foreground">
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
                <td className="py-2 pr-4">{i.displayName}</td>
                <td className="py-2 pr-4 font-mono text-xs">{i.username || ""}</td>
                <td className="py-2 pr-4">{i.identityType}</td>
                <td className="py-2 pr-4">
                  {(rolesByIdentity.get(i.id) ?? []).length === 0 ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <div className="flex flex-wrap gap-1">
                      {(rolesByIdentity.get(i.id) ?? []).map((rn, idx) => (
                        <span
                          key={idx}
                          className="inline-flex items-center rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground"
                        >
                          {rn}
                        </span>
                      ))}
                    </div>
                  )}
                </td>
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

      {/* Row-edit modal: display name + local credential (username prefilled,
          password set/change wired to SetLocalCredential, auth:write). */}
      <dialog
        ref={editDialogRef}
        onClose={handleEditCancel}
        className={cn(
          "rounded-2xl glass-menu text-foreground p-0 shadow-2xl backdrop:bg-black/50",
          "w-full max-w-md",
        )}
        onClick={(e) => {
          if (e.target === editDialogRef.current) handleEditCancel();
        }}
      >
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (editingId) handleEditSave(editingId);
          }}
          className="p-6"
        >
          <h2 className="text-lg font-semibold mb-4">Edit identity</h2>
          <div className="space-y-4">
            <div>
              <label htmlFor="edit-name" className="block text-sm font-medium mb-1.5">
                Display name
              </label>
              <Input
                id="edit-name"
                value={editName}
                onChange={(e) => setEditName(e.target.value)}
                autoFocus
              />
            </div>
            <div>
              <label htmlFor="edit-username" className="block text-sm font-medium mb-1.5">
                Username <span className="text-muted-foreground">(login handle)</span>
              </label>
              <Input
                id="edit-username"
                value={editUsername}
                onChange={(e) => setEditUsername(e.target.value)}
                placeholder="you@example.com"
                autoComplete="off"
              />
            </div>
            <div>
              <label htmlFor="edit-password" className="block text-sm font-medium mb-1.5">
                New password <span className="text-muted-foreground">(optional)</span>
              </label>
              <Input
                id="edit-password"
                type="password"
                value={editPassword}
                onChange={(e) => setEditPassword(e.target.value)}
                placeholder="Set or change the local login password"
                autoComplete="new-password"
              />
              <p className="mt-1 text-xs text-muted-foreground">
                Saving a username + password sets or changes the embedded-IdP
                login credential (admin-only, auth:write). Leave the password
                empty to only update the display name. The username is the
                login handle, which need not equal the identity subject.
              </p>
            </div>
          </div>
          <div className="mt-6 flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={handleEditCancel}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={!editName.trim() || update.isPending || setCredential.isPending}
            >
              {update.isPending || setCredential.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </form>
      </dialog>
    </div>
  );
}

// The exact resource set the RBAC interceptor knows (internal/rbac/rbac.go
// serviceToResource) + the 4 granular actions. The picker must only ever
// emit strings the interceptor enforces (resource:read / resource:write /
// these granular actions / *), never dead strings like "project:create".
const RBAC_RESOURCES = [
  "project",
  "workitem",
  "worker",
  "workflow",
  "policy",
  "recovery",
  "execution",
  "adapter",
  "telemetry",
  "aigateway",
  "auth",
  "webhook",
  "runtimeimage",
] as const;

const RBAC_GRANULAR_ACTIONS = [
  "policy:supersede",
  "policy:publish",
  "worker:publish",
  "workflow:publish",
] as const;

// EntitlementPicker renders grouped read/write toggles per resource plus a
// granular-actions group and an "Admin — all entitlements (*)" master toggle.
// Emits exactly the entitlement strings the interceptor enforces.
function EntitlementPicker({
  value,
  onChange,
}: {
  value: string[];
  onChange: (next: string[]) => void;
}) {
  // `admin` is derived from the current `value`, never kept as independent
  // state, so it always reflects the entitlements actually set. The dialog
  // stays mounted across create/edit sessions (no `key`), so storing this
  // in state would leave the Admin checkbox and disabled fieldset stale
  // (e.g. toggled on, cancelled, then Create reopened for a non-admin role).
  const admin = value.length === 1 && value[0] === "*";
  const set = (e: string, on: boolean) => {
    onChange(on ? [...new Set([...value, e])] : value.filter((v) => v !== e));
  };

  const toggleAdmin = (on: boolean) => {
    onChange(on ? ["*"] : []);
  };

  return (
    <div className="space-y-4">
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          checked={admin}
          onChange={(e) => toggleAdmin(e.target.checked)}
          className="h-4 w-4 rounded border-input"
        />
        <span className="font-medium">Admin — all entitlements (*)</span>
        <span className="text-xs text-muted-foreground">
          Grants every entitlement; disables the per-resource toggles.
        </span>
      </label>

      <fieldset disabled={admin} className="space-y-3">
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2 xl:grid-cols-3">
          {RBAC_RESOURCES.map((res) => (
            <div
              key={res}
              className="flex items-center justify-between rounded-xl glass-input px-3 py-2"
            >
              <span className="text-sm font-medium">{res}</span>
              <div className="flex items-center gap-3">
                <label className="flex items-center gap-1 text-xs">
                  <input
                    type="checkbox"
                    checked={value.includes(`${res}:read`)}
                    onChange={(e) => set(`${res}:read`, e.target.checked)}
                    className="h-3.5 w-3.5 rounded border-input"
                  />
                  read
                </label>
                <label className="flex items-center gap-1 text-xs">
                  <input
                    type="checkbox"
                    checked={value.includes(`${res}:write`)}
                    onChange={(e) => set(`${res}:write`, e.target.checked)}
                    className="h-3.5 w-3.5 rounded border-input"
                  />
                  write
                </label>
              </div>
            </div>
          ))}
        </div>
        <div>
          <p className="mb-2 text-xs font-semibold text-muted-foreground">
            Granular actions
          </p>
          <div className="flex flex-wrap gap-2">
            {RBAC_GRANULAR_ACTIONS.map((a) => (
              <label
                key={a}
                className="flex items-center gap-1.5 rounded-xl glass-input px-3 py-1.5 text-xs"
              >
                <input
                  type="checkbox"
                  checked={value.includes(a)}
                  onChange={(e) => set(a, e.target.checked)}
                  className="h-3.5 w-3.5 rounded border-input"
                />
                {a}
              </label>
            ))}
          </div>
        </div>
      </fieldset>
    </div>
  );
}

// entitlementChips renders human-readable chips: "resource:read"/"write"
// become "resource read"/"resource write"; granular actions keep their
// full resource:action form; "*" renders as "admin (*)".
function entitlementChips(ents: string[]) {
  const rows: { key: string; label: string }[] = [];
  for (const e of ents ?? []) {
    if (e === "*") {
      rows.push({ key: e, label: "admin (*)" });
    } else {
      const [res, action] = e.split(":");
      rows.push({ key: e, label: action === "write" || action === "read" ? `${res} ${action}` : e });
    }
  }
  if (rows.length === 0) {
    return <span className="text-muted-foreground">No entitlements</span>;
  }
  return (
    <div className="flex flex-wrap gap-1">
      {rows.map((r) => (
        <span
          key={r.key}
          className="inline-flex items-center rounded-full bg-muted px-2 py-0.5 text-xs text-muted-foreground"
        >
          {r.label}
        </span>
      ))}
    </div>
  );
}

function RolesTab() {
  const { data: roles } = useListRoles();
  const { data: identities } = useListIdentities();
  const { data: bindings } = useListRoleBindings();
  const createRole = useCreateRole();
  const updateRole = useUpdateRole();
  const deleteRole = useDeleteRole();
  const assignRole = useAssignRole();
  const revokeRole = useRevokeRole();
  const toast = useToast();

  // Create / edit role modal state.
  const [editing, setEditing] = useState<{ id?: string; name: string } | null>(null);
  const [formName, setFormName] = useState("");
  const [formEnts, setFormEnts] = useState<string[]>([]);
  const roleDialogRef = useRef<HTMLDialogElement>(null);

  // Manage Identity Roles state.
  const [mgrIdentityId, setMgrIdentityId] = useState("");
  const [mgrRoleId, setMgrRoleId] = useState("");

  useEffect(() => {
    const dialog = roleDialogRef.current;
    if (!dialog) return;
    if (editing) {
      dialog.showModal();
    } else {
      dialog.close();
    }
  }, [editing]);

  const roleName = useCallback(
    (id: string) => roles?.find((r) => r.id === id)?.name ?? id,
    [roles]
  );

  const identityName = useCallback(
    (id: string) =>
      identities?.find((i) => i.id === id)?.displayName ??
      identities?.find((i) => i.id === id)?.subject ??
      id,
    [identities]
  );

  // Role bindings of the selected identity (by id), used to render the
  // already-bound roles and drive the revoke picker.
  const selectedBindings = useMemo(
    () => (bindings ?? []).filter((b) => b.identityId === mgrIdentityId),
    [bindings, mgrIdentityId]
  );

  // Role names the selected identity already holds — excluded from the
  // assign picker so the admin cannot double-assign.
  const boundRoleNames = useMemo(() => {
    if (!mgrIdentityId) return new Set<string>();
    return new Set(selectedBindings.map((b) => roleName(b.roleId)));
  }, [selectedBindings, mgrIdentityId, roleName]);

  const assignableRoles = useMemo(
    () => (roles ?? []).filter((r) => !boundRoleNames.has(r.name)),
    [roles, boundRoleNames]
  );

  function startCreate() {
    setEditing({ id: "", name: "" });
    setFormName("");
    setFormEnts([]);
  }

  function startEdit(id: string) {
    const r = roles?.find((x) => x.id === id);
    if (!r) return;
    setEditing({ id, name: r.name });
    setFormName(r.name);
    setFormEnts(r.entitlements ?? []);
  }

  function cancelRole() {
    setEditing(null);
  }

  async function handleSaveRole() {
    const name = formName.trim();
    if (!name || !editing) return;
    try {
      if (editing.id) {
        await updateRole.mutateAsync({ id: editing.id, name, entitlements: formEnts });
        toast.success(`Role "${name}" updated.`);
      } else {
        await createRole.mutateAsync({ name, scope: "tenant", entitlements: formEnts });
        toast.success(`Role "${name}" created.`);
      }
      cancelRole();
    } catch {
      // error already toasted
    }
  }

  async function handleDelete(id: string, name: string) {
    if (!window.confirm(`Delete role "${name}"? This removes the role and its assignments and cannot be undone.`)) return;
    try {
      await deleteRole.mutateAsync(id);
      toast.success(`Role "${name}" deleted.`);
    } catch {
      // error already toasted
    }
  }

  async function handleAssign() {
    if (!mgrIdentityId || !mgrRoleId) return;
    try {
      await assignRole.mutateAsync({ identityId: mgrIdentityId, roleId: mgrRoleId, scope: "tenant" });
      toast.success(`Assigned "${roleName(mgrRoleId)}" to ${identityName(mgrIdentityId)}.`);
      setMgrRoleId("");
    } catch {
      // error already toasted
    }
  }

  async function handleRevoke(bindingId: string) {
    try {
      await revokeRole.mutateAsync(bindingId);
      toast.success("Role revoked.");
    } catch {
      // error already toasted
    }
  }

  return (
    <div className="space-y-6">
      {/* Create / edit role */}
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold">Roles</h3>
        <Button size="sm" onClick={startCreate}>
          New role
        </Button>
      </div>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="py-2 pr-4">Name</th>
              <th className="py-2 pr-4">Scope</th>
              <th className="py-2 pr-4">Entitlements</th>
              <th className="py-2 pr-4">Actions</th>
            </tr>
          </thead>
          <tbody>
            {(roles ?? []).length === 0 && (
              <tr>
                <td colSpan={4} className="py-3 text-muted-foreground">
                  No roles yet.
                </td>
              </tr>
            )}
            {(roles ?? []).map((r) => (
              <tr key={r.id} className="border-b">
                <td className="py-2 pr-4">{r.name}</td>
                <td className="py-2 pr-4">{r.scope}</td>
                <td className="py-2 pr-4">{entitlementChips(r.entitlements ?? [])}</td>
                <td className="py-2 pr-4 space-x-2 whitespace-nowrap">
                  <Button size="sm" variant="outline" onClick={() => startEdit(r.id)} disabled={r.name === "admin"}>
                    Edit
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    onClick={() => handleDelete(r.id, r.name)}
                    disabled={r.name === "admin"}
                    title={r.name === "admin" ? "The admin role cannot be deleted" : "Delete role"}
                  >
                    Delete
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Manage Identity Roles</h3>
        <p className="text-xs text-muted-foreground">
          Assign and revoke roles on identities using human-readable pickers.
        </p>
        <div className="flex flex-wrap gap-2">
          <select
            className="h-9 rounded-xl glass-input px-3 py-1.5 text-sm"
            value={mgrIdentityId}
            onChange={(e) => {
              setMgrIdentityId(e.target.value);
              setMgrRoleId("");
            }}
            aria-label="Identity"
          >
            <option value="">Select identity…</option>
            {(identities ?? []).map((i) => (
              <option key={i.id} value={i.id}>
                {i.displayName || i.subject} {i.subject && i.displayName ? `(${i.subject})` : ""}
              </option>
            ))}
          </select>
          <select
            className="h-9 rounded-xl glass-input px-3 py-1.5 text-sm"
            value={mgrRoleId}
            onChange={(e) => setMgrRoleId(e.target.value)}
            aria-label="Role"
          >
            <option value="">Select role…</option>
            {assignableRoles.map((r) => (
              <option key={r.id} value={r.id}>
                {r.name}
              </option>
            ))}
          </select>
          <Button
            onClick={handleAssign}
            disabled={!mgrIdentityId || !mgrRoleId || assignRole.isPending}
          >
            {assignRole.isPending ? "Assigning…" : "Assign"}
          </Button>
        </div>

        {mgrIdentityId && (
          <div className="mt-2">
            <p className="mb-2 text-xs font-medium text-muted-foreground">
              Roles bound to {identityName(mgrIdentityId)}:
            </p>
            {selectedBindings.length === 0 ? (
              <p className="text-sm text-muted-foreground">No roles assigned.</p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {selectedBindings.map((b) => (
                  <div
                    key={b.id}
                    className="flex items-center gap-2 rounded-full bg-muted px-3 py-1 text-xs"
                  >
                    <span>{roleName(b.roleId)}</span>
                    <button
                      onClick={() => handleRevoke(b.id)}
                      disabled={revokeRole.isPending}
                      className="text-muted-foreground hover:text-destructive"
                      title="Revoke role"
                      aria-label={`Revoke ${roleName(b.roleId)}`}
                    >
                      ×
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Create / edit role modal */}
      <dialog
        ref={roleDialogRef}
        onClose={cancelRole}
        className={cn(
          "rounded-2xl glass-menu text-foreground p-0 shadow-2xl backdrop:bg-black/50",
          "w-full max-w-2xl",
        )}
        onClick={(e) => {
          if (e.target === roleDialogRef.current) cancelRole();
        }}
      >
        <form
          onSubmit={(e) => {
            e.preventDefault();
            handleSaveRole();
          }}
          className="p-6"
        >
          <h2 className="text-lg font-semibold mb-4">
            {editing?.id ? `Edit role "${editing.name}"` : "Create role"}
          </h2>
          <div className="space-y-4">
            <div>
              <label htmlFor="role-name" className="block text-sm font-medium mb-1.5">
                Name
              </label>
              <Input
                id="role-name"
                value={formName}
                onChange={(e) => setFormName(e.target.value)}
                placeholder="e.g. project-editor"
                autoFocus
              />
              <p className="mt-1 text-xs text-muted-foreground">
                Must match <code className="font-mono">^[a-z0-9]+(?:-[a-z0-9]+)*$</code>.
              </p>
            </div>
            <div>
              <p className="mb-1.5 text-sm font-medium">Entitlements</p>
              <EntitlementPicker value={formEnts} onChange={setFormEnts} />
            </div>
          </div>
          <div className="mt-6 flex justify-end gap-2">
            <Button type="button" variant="outline" onClick={cancelRole}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={!formName.trim() || createRole.isPending || updateRole.isPending}
            >
              {createRole.isPending || updateRole.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </form>
      </dialog>
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
            className="mt-1 h-9 w-52 rounded-xl glass-input px-3 text-sm"
          />
        </div>
        <div>
          <Label htmlFor="audit-to">To (local time)</Label>
          <input
            id="audit-to"
            type="datetime-local"
            value={draft.to}
            onChange={set("to")}
            className="mt-1 h-9 w-52 rounded-xl glass-input px-3 text-sm"
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
