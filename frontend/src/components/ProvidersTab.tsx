// Providers tab (ADR-0006): tenant provider management under
// Settings → Adapters. Lists built-in profiles (read-only entries) and
// tenant custom providers (first-class CRUD), with enable/disable toggles,
// baseURL overrides, token auto-write (never a manual secrets visit),
// Ollama context settings, per-provider model visibility, manual model
// entries (custom providers), and the deletion guard (blocked while
// workers reference the provider).
import { useState } from "react";

import {
  useProviderList,
  useUpdateProviderSettings,
  useCreateCustomProvider,
  useUpdateCustomProvider,
  useDeleteCustomProvider,
  useSetProviderToken,
  useClearProviderToken,
  useProviderModels,
} from "@/api/providers";
import type { ProviderEntry } from "@/api/gen/orchicon/api/v1/provider_pb";
import { ProviderManualModel } from "@/api/gen/orchicon/api/v1/provider_pb";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Loader2, Plus, Trash2, AlertTriangle, KeyRound, Eye, Pencil, X } from "lucide-react";

const OLLAMA_ID = "ollama";

// ManualModelsEditor manages the operator-added manual model entries on a
// custom provider (ADR-0006 D1/D4): model id + optional context-window /
// max-output / reasoning hints. The whole list is persisted with
// replaceManualModels=true (the merge logic lives in the service, not the
// UI). Rendered under the same "models" tier as the probe results.
function ManualModelsEditor({ entry }: { entry: ProviderEntry }) {
  const [models, setModels] = useState<ProviderManualModel[]>(
    () => (entry.manualModels ?? []).map((m) => new ProviderManualModel({ ...m })),
  );
  const [newId, setNewId] = useState("");
  const [newCtx, setNewCtx] = useState("");
  const [newMaxOut, setNewMaxOut] = useState("");
  const [newReasoning, setNewReasoning] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const updateSettings = useUpdateProviderSettings();

  function add() {
    setMsg(null);
    const id = newId.trim();
    if (!id) return;
    const model = new ProviderManualModel({
      id,
      context: BigInt(newCtx.trim() === "" ? 0 : Number(newCtx)),
      maxOutput: BigInt(newMaxOut.trim() === "" ? 0 : Number(newMaxOut)),
      reasoning: newReasoning,
    });
    setModels((prev) => (prev.some((m) => m.id === id) ? prev : [...prev, model]));
    setNewId("");
    setNewCtx("");
    setNewMaxOut("");
    setNewReasoning(false);
  }

  function updateRow(id: string, patch: Partial<ProviderManualModel>) {
    setModels((prev) => prev.map((m) => (m.id === id ? new ProviderManualModel({ ...m, ...patch }) : m)));
  }

  function removeRow(id: string) {
    setModels((prev) => prev.filter((m) => m.id !== id));
  }

  function save() {
    setMsg(null);
    updateSettings.mutate(
      {
        providerId: entry.id,
        manualModels: models.map((m) => ({
          id: m.id,
          context: m.context,
          maxOutput: m.maxOutput,
          reasoning: m.reasoning,
        })),
        replaceManualModels: true,
      },
      {
        onSuccess: () => setMsg("Manual models saved."),
        onError: (e) => setMsg(String(e)),
      },
    );
  }

  return (
    <div className="space-y-2">
      <p className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
        Manual models (custom provider)
      </p>
      {models.length > 0 && (
        <ul className="space-y-1">
          {models.map((m) => (
            <li key={m.id} className="flex items-center gap-2 text-xs">
              <span className="min-w-0 flex-1 truncate">
                <code>{m.id}</code>
              </span>
              <label className="flex items-center gap-1 text-muted-foreground">
                ctx
                <Input
                  type="number"
                  min={0}
                  className="h-6 w-20 px-1 text-xs"
                  value={m.context.toString()}
                  onChange={(e) => updateRow(m.id, { context: BigInt(Number(e.target.value) || 0) })}
                />
              </label>
              <label className="flex items-center gap-1 text-muted-foreground">
                max
                <Input
                  type="number"
                  min={0}
                  className="h-6 w-20 px-1 text-xs"
                  value={m.maxOutput.toString()}
                  onChange={(e) => updateRow(m.id, { maxOutput: BigInt(Number(e.target.value) || 0) })}
                />
              </label>
              <label className="flex items-center gap-1 text-muted-foreground">
                <input
                  type="checkbox"
                  checked={m.reasoning}
                  onChange={(e) => updateRow(m.id, { reasoning: e.target.checked })}
                />
                reasoning
              </label>
              <Button size="icon" variant="ghost" className="h-6 w-6" onClick={() => removeRow(m.id)} aria-label={`Remove ${m.id}`}>
                <X className="h-3 w-3" />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Input
          placeholder="model id (e.g. Qwen3.6-35B-A3B)"
          value={newId}
          onChange={(e) => setNewId(e.target.value)}
          className="h-7 w-44 text-xs"
        />
        <Input
          type="number"
          min={0}
          placeholder="ctx"
          value={newCtx}
          onChange={(e) => setNewCtx(e.target.value)}
          className="h-7 w-16 text-xs"
          aria-label="context window"
        />
        <Input
          type="number"
          min={0}
          placeholder="max"
          value={newMaxOut}
          onChange={(e) => setNewMaxOut(e.target.value)}
          className="h-7 w-16 text-xs"
          aria-label="max output"
        />
        <label className="flex items-center gap-1 text-[10px] text-muted-foreground">
          <input type="checkbox" checked={newReasoning} onChange={(e) => setNewReasoning(e.target.checked)} />
          reasoning
        </label>
        <Button size="sm" variant="outline" className="h-7" onClick={add} disabled={!newId.trim()}>
          <Plus className="h-3 w-3" /> Add
        </Button>
        <Button size="sm" className="h-7" onClick={save} disabled={updateSettings.isPending}>
          {updateSettings.isPending ? <Loader2 className="h-3 w-3 animate-spin" /> : "Save"}
        </Button>
      </div>
      {msg && <p className="text-xs text-muted-foreground">{msg}</p>}
    </div>
  );
}

// ProviderCard renders one merged provider entry. Built-ins render their
// controls but never delete/edit-identity (read-only definition).
function ProviderCard({ entry }: { entry: ProviderEntry }) {
  const [token, setToken] = useState("");
  const [baseUrl, setBaseUrl] = useState(entry.baseUrlOverride || "");
  const [numCtx, setNumCtx] = useState<string>(entry.numCtxDefault ? entry.numCtxDefault.toString() : "");
  const [showModels, setShowModels] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  // Batch visibility editing (ADR-0006 D4 operator flow): checkbox toggles
  // stage a draft hidden-set; the explicit Save button commits
  // hiddenModels in ONE update. null = no draft open (checkboxes mirror
  // entry.hiddenModels); non-null = the draft set being edited.
  const [draftHidden, setDraftHidden] = useState<Set<string> | null>(null);

  const updateSettings = useUpdateProviderSettings();
  const setTokenMut = useSetProviderToken();
  const clearTokenMut = useClearProviderToken();
  const deleteMut = useDeleteCustomProvider();
  const modelsQ = useProviderModels(entry.id, showModels);

  function saveToggle(enabled: boolean) {
    setMsg(null);
    updateSettings.mutate({ providerId: entry.id, enabled }, { onError: (e) => setMsg(String(e)) });
  }
  function saveBaseUrl() {
    setMsg(null);
    updateSettings.mutate({ providerId: entry.id, baseUrlOverride: baseUrl }, { onError: (e) => setMsg(String(e)) });
  }
  function saveNumCtx() {
    setMsg(null);
    const n = numCtx.trim() === "" ? 0 : Number(numCtx);
    // numCtxDefault is proto int64 → TS bigint; coerce explicitly.
    updateSettings.mutate(
      { providerId: entry.id, numCtxDefault: BigInt(n > 0 ? n : 0) },
      { onError: (e) => setMsg(String(e)) },
    );
  }
  function saveToken() {
    setMsg(null);
    if (!token.trim()) return;
    setTokenMut.mutate(
      { providerId: entry.id, token: token.trim() },
      {
        onSuccess: (res) => {
          setToken("");
          setMsg(`Saved — secret ${res.secretName} written automatically.`);
        },
        onError: (e) => setMsg(String(e)),
      },
    );
  }
  function removeToken() {
    setMsg(null);
    clearTokenMut.mutate({ providerId: entry.id }, { onError: (e) => setMsg(String(e)) });
  }
  function remove() {
    setMsg(null);
    deleteMut.mutate(
      { refId: entry.id },
      {
        onSuccess: () => setMsg(null),
        onError: (e) => setMsg(String(e)),
      },
    );
  }

  // Draft visibility editing: checkbox source of truth while a draft is
  // open; Save commits once, Discard reverts the staged set.
  const storedHidden = new Set(entry.hiddenModels ?? []);
  const currentHidden = draftHidden ?? storedHidden;

  function toggleModel(id: string, visible: boolean) {
    setMsg(null);
    const next = new Set(currentHidden);
    if (visible) {
      next.delete(id);
    } else {
      next.add(id);
    }
    setDraftHidden(next);
  }

  function saveVisibility() {
    setMsg(null);
    if (draftHidden === null) return;
    updateSettings.mutate(
      { providerId: entry.id, hiddenModels: [...draftHidden] },
      {
        onSuccess: () => {
          setDraftHidden(null);
          setMsg("Model visibility saved.");
        },
        onError: (e) => setMsg(String(e)),
      },
    );
  }

  function discardVisibility() {
    setDraftHidden(null);
  }

  return (
    <Card className={entry.enabled ? "" : "opacity-70"}>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center justify-between text-base">
          <span className="flex items-center gap-2">
            {entry.displayName || entry.id}
            {entry.isCustom && (
              <span className="rounded bg-purple-100 px-1 py-0.5 text-[10px] font-medium text-purple-700">custom</span>
            )}
            {entry.readOnly && (
              <span className="rounded bg-muted px-1 py-0.5 text-[10px] text-muted-foreground">built-in</span>
            )}
          </span>
          <span className="flex items-center gap-2">
            {entry.isCustom && (
              <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" onClick={() => setEditing(true)}>
                <Pencil className="mr-1 h-3 w-3" /> Edit
              </Button>
            )}
            <label className="flex items-center gap-2 text-xs font-normal">
              <input
                type="checkbox"
                checked={entry.enabled}
                onChange={(e) => saveToggle(e.target.checked)}
                aria-label={`Enable ${entry.id}`}
              />
              enabled
            </label>
          </span>
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <p className="text-xs text-muted-foreground">
          ref: <code>{entry.id}</code> · kind: {entry.kind}
        </p>

        {/* Base URL override (built-ins) or definition (customs). */}
        <div className="flex items-center gap-2">
          <label className="w-24 shrink-0 text-xs text-muted-foreground">Base URL</label>
          <Input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder={entry.baseUrl || "https://..."} />
          {entry.id === "ollama" && (
            <p className="text-[10px] text-muted-foreground">
              Default hosts a local daemon (http://localhost:11434). For Ollama Cloud set the cloud endpoint here,
              e.g. https://ollama.com — models then list from the cloud account.
            </p>
          )}
          <Button size="sm" variant="outline" onClick={saveBaseUrl} disabled={updateSettings.isPending}>
            Save
          </Button>
        </div>

        {/* Token auto-write: standard secret name, shown but never the value. */}
        {entry.authMode !== "none" && (
          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <label className="flex w-24 shrink-0 items-center gap-1 text-xs text-muted-foreground">
                <KeyRound className="h-3 w-3" /> API token
              </label>
              <Input
                type="password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder={entry.hasTokenStored ? "stored (enter to replace)" : "paste token…"}
              />
              <Button size="sm" variant="outline" onClick={saveToken} disabled={setTokenMut.isPending || !token.trim()}>
                {setTokenMut.isPending ? <Loader2 className="h-3 w-3 animate-spin" /> : "Save"}
              </Button>
              {entry.hasTokenStored && (
                <Button size="sm" variant="ghost" onClick={removeToken} disabled={clearTokenMut.isPending}>
                  Remove
                </Button>
              )}
            </div>
            <p className="text-[10px] text-muted-foreground">
              Writes tenant secret <code>{entry.tokenSecretName || "(auto-named)"}</code> automatically — no manual
              secrets visit needed.
            </p>
          </div>
        )}

        {/* Ollama context settings. */}
        {entry.id === OLLAMA_ID && (
          <div className="flex items-center gap-2">
            <label className="w-24 shrink-0 text-xs text-muted-foreground">num_ctx</label>
            <Input
              type="number"
              min={0}
              value={numCtx}
              onChange={(e) => setNumCtx(e.target.value)}
              placeholder="default"
              className="w-32"
            />
            <Button size="sm" variant="outline" onClick={saveNumCtx} disabled={updateSettings.isPending}>
              Save
            </Button>
          </div>
        )}

        {/* Sourcing surface: probed ⊕ manual, deduped; visibility toggles;
            manual-model editor for customs. */}
        <div className="space-y-1">
          <button
            type="button"
            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            onClick={() => setShowModels((v) => !v)}
          >
            <Eye className="h-3 w-3" /> {showModels ? "Hide" : "Show"} models
          </button>
          {showModels && (
            <div className="space-y-2 rounded border p-2">
              {entry.isCustom && <ManualModelsEditor entry={entry} />}
              <div className="space-y-1">
                {modelsQ.isLoading && <Loader2 className="h-3 w-3 animate-spin" />}
                {modelsQ.data?.degraded && (
                  <p className="flex items-center gap-1 text-xs text-amber-600">
                    <AlertTriangle className="h-3 w-3" /> Probe failed — showing manual entries only (non-fatal).
                  </p>
                )}
                {modelsQ.data?.enabled === false && <p className="text-xs text-muted-foreground">Provider disabled.</p>}
                {(modelsQ.data?.models ?? []).map((m) => (
                  <label key={m.id} className="flex items-center gap-2 text-xs">
                    <input
                      type="checkbox"
                      checked={!currentHidden.has(m.id)}
                      onChange={(e) => toggleModel(m.id, e.target.checked)}
                    />
                    <code>{m.id}</code>
                    {m.source === "manual" && <span className="rounded bg-muted px-1 text-[10px]">manual</span>}
                    {m.warnNoContext && (
                      <span
                        className="rounded bg-amber-100 px-1 text-[10px] font-medium text-amber-700"
                        title="No context-window hint — selectable but may misbehave"
                      >
                        WARN: no context hint
                      </span>
                    )}
                  </label>
                ))}
                {draftHidden !== null && (
                  <div className="flex items-center gap-2 pt-1">
                    <Button size="sm" className="h-7" onClick={saveVisibility} disabled={updateSettings.isPending}>
                      {updateSettings.isPending ? <Loader2 className="h-3 w-3 animate-spin" /> : "Save"}
                    </Button>
                    <Button size="sm" variant="ghost" className="h-7" onClick={discardVisibility} disabled={updateSettings.isPending}>
                      Discard
                    </Button>
                    <span className="text-[10px] text-muted-foreground">Selection staged — click Save to apply.</span>
                  </div>
                )}
                {modelsQ.data && (modelsQ.data.models ?? []).length === 0 && !modelsQ.data.degraded && (
                  <p className="text-xs text-muted-foreground">No models discovered.</p>
                )}
              </div>
            </div>
          )}
        </div>

        {msg && <p className="text-xs text-destructive">{msg}</p>}

        {entry.isCustom && (
          <Button size="sm" variant="destructive" onClick={remove} disabled={deleteMut.isPending}>
            <Trash2 className="mr-1 h-3 w-3" /> Delete
          </Button>
        )}
      </CardContent>

      {editing && entry.isCustom && (
        <EditCustomDialog
          entry={entry}
          onClose={() => setEditing(false)}
          onError={(e) => setMsg(String(e))}
        />
      )}
    </Card>
  );
}

// EditCustomDialog edits the mutable definition of a custom provider
// (display name, base URL, auth mode). The ref id is immutable after
// create — it is the model_ref segment-2 identity.
function EditCustomDialog({
  entry,
  onClose,
  onError,
}: {
  entry: ProviderEntry;
  onClose: () => void;
  onError: (e: unknown) => void;
}) {
  const [displayName, setDisplayName] = useState(entry.displayName || "");
  const [baseUrl, setBaseUrl] = useState(entry.baseUrl || "");
  const [authMode, setAuthMode] = useState<"none" | "token">(entry.authMode === "token" ? "token" : "none");
  const [err, setErr] = useState<string | null>(null);
  const updateMut = useUpdateCustomProvider();

  function submit() {
    setErr(null);
    updateMut.mutate(
      { refId: entry.id, displayName, baseUrl, authMode },
      {
        onSuccess: onClose,
        onError: (e) => {
          setErr(String(e));
          onError(e);
        },
      },
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40" role="dialog" aria-label={`Edit ${entry.id}`}>
      <div className="w-full max-w-md space-y-3 rounded-lg border bg-background p-4">
        <h2 className="text-base font-semibold">
          Edit custom provider <span className="font-mono text-xs text-muted-foreground">{entry.id}</span>
        </h2>
        <p className="text-xs text-muted-foreground">ref id is immutable after create.</p>
        <Input placeholder="display name (optional)" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        <Input placeholder="base URL (http(s)://…)" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
        <p className="text-[10px] text-muted-foreground">
          OpenAI-compatible base INCLUDES the version root (…/v1). From inside the control-plane
          container, localhost/0.0.0.0 are the CONTAINER — use the docker bridge IP (172.17.0.1), the
          host LAN IP, or a mapped port. Models list from {"{"}base{"}"}/models.
        </p>
        <label className="flex items-center gap-2 text-xs">
          auth mode
          <select
            className="rounded-md border bg-background px-2 py-1 text-xs text-foreground"
            value={authMode}
            onChange={(e) => setAuthMode(e.target.value as "none" | "token")}
          >
            <option value="none" className="text-foreground">none</option>
            <option value="token" className="text-foreground">token (auto-writes CUSTOM_&lt;REF&gt;_API_KEY)</option>
          </select>
        </label>
        {err && <p className="text-xs text-destructive">{err}</p>}
        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button size="sm" onClick={submit} disabled={updateMut.isPending || !baseUrl}>
            {updateMut.isPending ? <Loader2 className="h-3 w-3 animate-spin" /> : "Save"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// CreateDialog collects the custom provider definition (display name,
// ref id, baseURL, auth mode).
function CreateDialog({ onClose }: { onClose: () => void }) {
  const [refId, setRefId] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [authMode, setAuthMode] = useState<"none" | "token">("none");
  const [err, setErr] = useState<string | null>(null);
  const createMut = useCreateCustomProvider();

  function submit() {
    setErr(null);
    createMut.mutate(
      { refId, displayName, baseUrl, authMode },
      {
        onSuccess: onClose,
        onError: (e) => setErr(String(e)),
      },
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40" role="dialog" aria-label="Add custom provider">
      <div className="w-full max-w-md space-y-3 rounded-lg border bg-background p-4">
        <h2 className="text-base font-semibold">Add custom provider</h2>
        <p className="text-xs text-muted-foreground">
          Any OpenAI-compatible endpoint. ref id joins the model_ref grammar (e.g.{" "}
          <code>orchicon/local-models/&lt;model&gt;</code>).
        </p>
        <Input placeholder="ref id (e.g. local-models)" value={refId} onChange={(e) => setRefId(e.target.value)} />
        <Input placeholder="display name (optional)" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        <Input placeholder="base URL (http(s)://…)" value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
        <p className="text-[10px] text-muted-foreground">
          OpenAI-compatible endpoint, base INCLUDES the version root (e.g. http://172.17.0.1:8095/v1 —
          llama-server style). From inside the control-plane container, localhost/0.0.0.0 are the CONTAINER,
          not your host: use the docker bridge IP (172.17.0.1), the host LAN IP, or run the server on 0.0.0.0
          plus <code>--host 0.0.0.0</code> and map the port. Models list from {"{"}base{"}"}/models.
        </p>
        <label className="flex items-center gap-2 text-xs">
          auth mode
          <select
            className="rounded-md border bg-background px-2 py-1 text-xs text-foreground"
            value={authMode}
            onChange={(e) => setAuthMode(e.target.value as "none" | "token")}
          >
            <option value="none" className="text-foreground">none</option>
            <option value="token" className="text-foreground">token (auto-writes CUSTOM_&lt;REF&gt;_API_KEY)</option>
          </select>
        </label>
        {err && <p className="text-xs text-destructive">{err}</p>}
        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button size="sm" onClick={submit} disabled={createMut.isPending || !refId || !baseUrl}>
            {createMut.isPending ? <Loader2 className="h-3 w-3 animate-spin" /> : "Create"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function ProvidersTab() {
  const { data: providers, isLoading, error } = useProviderList();
  const [creating, setCreating] = useState(false);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Providers</h2>
          <p className="text-sm text-muted-foreground">
            Enable providers, override base URLs, store tokens (auto-written to tenant secrets), and manage custom
            OpenAI-compatible endpoints.
          </p>
        </div>
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus className="mr-1 h-3 w-3" /> Add custom provider
        </Button>
      </div>

      {isLoading && <Loader2 className="h-4 w-4 animate-spin" />}
      {error && <p className="text-sm text-destructive">Failed to load providers: {String(error)}</p>}
      <div className="space-y-3">
        {(providers ?? []).map((p) => (
          <ProviderCard key={p.id} entry={p} />
        ))}
      </div>

      {creating && <CreateDialog onClose={() => setCreating(false)} />}
    </div>
  );
}
