// Providers tab (ADR-0006): tenant provider management under
// Settings → Adapters. Lists built-in profiles (read-only entries) and
// tenant custom providers (first-class CRUD), with enable/disable toggles,
// baseURL overrides, token auto-write (never a manual secrets visit),
// Ollama context settings, per-provider model visibility, and the
// deletion guard (blocked while workers reference the provider).
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
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Loader2, Plus, Trash2, AlertTriangle, KeyRound, Eye } from "lucide-react";

const OLLAMA_ID = "ollama";

// ProviderCard renders one merged provider entry. Built-ins render their
// controls but never delete/edit-identity (read-only definition).
function ProviderCard({ entry }: { entry: ProviderEntry }) {
  const [token, setToken] = useState("");
  const [baseUrl, setBaseUrl] = useState(entry.baseUrlOverride || "");
  const [numCtx, setNumCtx] = useState<string>(entry.numCtxDefault ? String(entry.numCtxDefault) : "");
  const [showModels, setShowModels] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);

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
    updateSettings.mutate({ providerId: entry.id, numCtxDefault: n > 0 ? n : 0 }, { onError: (e) => setMsg(String(e)) });
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
          <label className="flex items-center gap-2 text-xs font-normal">
            <input
              type="checkbox"
              checked={entry.enabled}
              onChange={(e) => saveToggle(e.target.checked)}
              aria-label={`Enable ${entry.id}`}
            />
            enabled
          </label>
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

        {/* Sourcing surface: probed ⊕ manual, deduped; visibility toggles. */}
        <div className="space-y-1">
          <button
            type="button"
            className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            onClick={() => setShowModels((v) => !v)}
          >
            <Eye className="h-3 w-3" /> {showModels ? "Hide" : "Show"} models
          </button>
          {showModels && (
            <div className="space-y-1 rounded border p-2">
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
                    checked={m.visible}
                    onChange={(e) => {
                      const hidden = e.target.checked
                        ? (entry.hiddenModels ?? []).filter((h) => h !== m.id)
                        : [...(entry.hiddenModels ?? []), m.id];
                      updateSettings.mutate({ providerId: entry.id, hiddenModels: hidden }, { onError: (err) => setMsg(String(err)) });
                    }}
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
              {modelsQ.data && (modelsQ.data.models ?? []).length === 0 && !modelsQ.data.degraded && (
                <p className="text-xs text-muted-foreground">No models discovered.</p>
              )}
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
    </Card>
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
        <label className="flex items-center gap-2 text-xs">
          auth mode
          <select value={authMode} onChange={(e) => setAuthMode(e.target.value as "none" | "token")}>
            <option value="none">none</option>
            <option value="token">token (auto-writes CUSTOM_&lt;REF&gt;_API_KEY)</option>
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
