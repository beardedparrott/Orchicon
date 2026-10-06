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
// A CREDENTIAL IS OFFERED AT EVERY SCOPE, and it is ATTACHED differently at each:
// an owned row stores it against its ID (the Credentials card below), while a
// worker version's inline spec has no row and gets the ${SECRET_NAME} reference
// built INTO its own env/headers (workerCredential) — the same split the TUI's
// scope modal makes with its `k` verb (internal/tui/scope_modal.go).
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
  usePrefillMCPCatalogEntry,
} from "@/api/mcpServers";
import { MCPServerTransport } from "@/api/gen/orchicon/api/v1/mcp_server_pb";
import { useCreateSecret, useSecretList, useUpdateSecret } from "@/api/secrets";
import { attachSecret, credentialKeyCandidates, inlineSpecIsHTTP, withReference } from "@/lib/mcpInlineCredential";
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
  /** The registry entry this form came from, EMPTY for a manual entry.
   *
   *  It is carried so the credential control can offer the key the entry DECLARES it needs (a catalog
   *  entry's requiredEnv) as the default — and so the create sends it, because the plane derives a row's
   *  required_secrets — and therefore its Install button and "secrets stored" badge — from the slug this
   *  request carries. Dropping it (which the panel used to do) left a catalog-added server with no
   *  declared keys and no Install control at all. */
  catalogSlug: string;
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
  catalogSlug: "",
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

// NEW_SELECTION is the sentinel a <select> uses for "type a new one".
//
// A SELECT CANNOT HOLD FREE TEXT, AND THAT IS THE POINT. The rule for credentials is "select what exists;
// typing is only for creating something new", and a picker enforces exactly that: the static case is a
// choice among real values (the keys the server declares, the names the store holds), and the new case is
// an explicit, separate choice that reveals its own field. The text boxes this replaced could not tell the
// two apart — every value looked typed, and a reference to a secret nobody stored was indistinguishable
// from one that resolves.
const NEW_SELECTION = "__new__";

// FORM_TARGET_ID names the SERVER BEING EDITED as a credential target.
//
// It exists because the edit page is where the operator reported typing credentials ("Credentials are still
// something you type in … we need the ability to select/create the secret credentials from the MCP edit
// page"). At that placement there is no row yet — the reference is written into the FORM's own env/headers
// and travels with the Create/Save that follows — so the target needs an id that cannot collide with a real
// server's.
const FORM_TARGET_ID = "__form__";

// CredentialTarget is one server a credential can be attached to, in either placement.
interface CredentialTarget {
  id: string;
  label: string;
  /** True when this server reads HEADERS rather than env (streamable HTTP). */
  isHTTP: boolean;
  env?: Record<string, string>;
  headers?: Record<string, string>;
  /** Keys the server DECLARES it needs (a catalog entry's requiredEnv), offered before its own. */
  declaredKeys?: string[];
}

// CredentialDraft is what the card hands back: a target, the key to fill, and the NAME of a stored secret —
// plus whether that name still has to be stored. The card never handles a VALUE it did not just receive
// for storing, and the value it does receive goes straight to the secrets store, never to the server.
interface CredentialDraft {
  targetId: string;
  key: string;
  secretName: string;
  createNew: boolean;
  newValue?: string;
}

// CredentialCard is the ONE credential control: a target, a key, and a stored secret, all chosen from
// lists — with a field appearing only where a list cannot answer (a new key, or a new secret).
// SecretCombobox is the SEARCHABLE half of the credential control: type to filter the store's names, pick
// one — or ask for a NEW secret, which is its own row at the foot of the list rather than a name the box
// guessed at. The operator asked for exactly this: "you should be able to search through current available
// secrets or 'create new secret'".
//
// IT IS A COMBOBOX AND NOT A DATALIST-BACKED TEXT BOX, because the options must be the ONLY thing that can be
// picked. A datalist suggests without constraining, so a typed name matching nothing looks exactly like one
// matching a stored secret — which is how "type a credential" creeps back in. Here every option is a real
// name, and the one row that is not says what it does.
//
// The list is RENDERED ALWAYS and hidden by class, so what the markup says and what the operator sees are
// the same set of names.
function SecretCombobox({
  names,
  value,
  disabled,
  onPick,
  onNew,
}: {
  names: string[];
  value: string;
  disabled?: boolean;
  onPick: (name: string) => void;
  onNew: (name: string) => void;
}) {
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const q = query.trim();
  const matches = q === "" ? names : names.filter((n) => n.toLowerCase().includes(q.toLowerCase()));
  return (
    <div className="relative">
      <Input
        placeholder="Search stored secrets…"
        value={query}
        disabled={disabled}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onChange={(e) => {
          setQuery(e.target.value);
          setOpen(true);
        }}
        // ENTER PICKS; IT MUST NEVER SUBMIT. This control is mounted inside the worker routes' draft-version
        // form element (and the add/edit form), and HTML's implicit submission fires on Enter in a text
        // field whenever the form has a submit control — which both hosts do. Left alone, typing a secret
        // name and pressing Enter would have SAVED the version or submitted the form: the same surprise
        // class as the reported bounce, arriving by keyboard instead of by click.
        //
        // (The wording avoids writing the tag itself: the form-nesting suite scans these files for form
        // tags, and a tag inside a comment would open a phantom form and flag every Button after it.)
        //
        // So Enter is consumed here and does the useful thing: an exact match is picked, a single match is
        // picked, and otherwise the list stays open for an explicit pick.
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          const exact = names.find((n) => n.toLowerCase() === q.toLowerCase());
          const pick = exact ?? (matches.length === 1 ? matches[0] : undefined);
          if (pick) {
            onPick(pick);
            setQuery(pick);
            setOpen(false);
          }
        }}
      />
      {/* onMouseDown, not onClick: the pick has to land BEFORE the input's blur closes the list. */}
      <div
        className={
          open
            ? "absolute z-50 mt-1 max-h-56 w-full overflow-auto rounded-md border border-input bg-muted shadow-lg"
            : "hidden"
        }
      >
        {matches.map((n) => (
          <button
            key={n}
            type="button"
            className="block w-full px-3 py-1.5 text-left text-sm hover:bg-accent"
            onMouseDown={(e) => {
              e.preventDefault();
              onPick(n);
              setQuery(n);
              setOpen(false);
            }}
          >
            {n}
          </button>
        ))}
        <button
          type="button"
          className="block w-full px-3 py-1.5 text-left text-sm text-muted-foreground hover:bg-accent"
          onMouseDown={(e) => {
            e.preventDefault();
            onNew(q);
            setOpen(false);
          }}
        >
          {q === "" ? "Create a new secret…" : `Create a new secret “${q}”…`}
        </button>
      </div>
      {value !== "" && !open && <p className="mt-1 text-xs text-muted-foreground">stored secret: {value}</p>}
    </div>
  );
}

// CredentialCard is the ONE credential control: a target, a key, and a stored secret — the key picked from
// what the server declares and already carries, the secret SEARCHED and picked from the store, and a field
// appearing only where a list cannot answer (a new key, or a new secret).
//
// IT RENDERS IN THE SERVER'S OWN EDIT FORM TOO (compact), because the operator's report was about the edit
// page: "Credentials are still something you type in … We need the ability to select/create the secret
// credentials from the MCP edit page". At that placement the target IS the form being filled in, and the
// reference it writes travels with the Create/Save that follows.
function CredentialCard({
  targets,
  secretNames,
  busy,
  onApply,
  description,
  note,
  compact = false,
}: {
  targets: CredentialTarget[];
  secretNames: string[];
  busy: boolean;
  onApply: (draft: CredentialDraft) => void;
  description: string;
  note?: string;
  compact?: boolean;
}) {
  const [targetId, setTargetId] = useState("");
  const [keyChoice, setKeyChoice] = useState("");
  const [newKey, setNewKey] = useState("");
  const [secretChoice, setSecretChoice] = useState("");
  const [newSecretName, setNewSecretName] = useState("");
  const [newSecretValue, setNewSecretValue] = useState("");

  const target = targets.find((t) => t.id === (targetId || targets[0]?.id)) ?? targets[0];
  const candidates = target
    ? credentialKeyCandidates(target.env, target.headers, target.isHTTP, target.declaredKeys ?? [])
    : [];
  // A server with no candidate keys has exactly one honest answer — a new one — so the picker defaults to
  // that rather than sitting on a value it cannot offer.
  const keyChoiceValue = keyChoice || candidates[0] || NEW_SELECTION;
  const resolvedKey = keyChoiceValue === NEW_SELECTION ? newKey.trim() : keyChoiceValue;
  const creatingSecret = secretChoice === NEW_SELECTION;
  const resolvedSecret = creatingSecret ? newSecretName.trim() : secretChoice;
  const keyLabel = target?.isHTTP ? "header" : "env var";

  const controls = (
    <>
      <div className="grid gap-2 sm:grid-cols-2">
        {targets.length > 1 ? (
          <select
            className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
            value={target?.id ?? ""}
            onChange={(e) => {
              setTargetId(e.target.value);
              // The candidate keys belong to the SERVER, so they are re-chosen with it.
              setKeyChoice("");
              setNewKey("");
            }}
          >
            {targets.map((t) => (
              <option key={t.id} value={t.id}>
                {t.label}
              </option>
            ))}
          </select>
        ) : (
          <div className="flex h-9 items-center text-sm text-muted-foreground">{target?.label}</div>
        )}

        <select
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
          value={keyChoiceValue}
          onChange={(e) => setKeyChoice(e.target.value)}
        >
          {candidates.map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
          <option value={NEW_SELECTION}>Add a new {keyLabel}…</option>
        </select>

        {keyChoiceValue === NEW_SELECTION && (
          <Input placeholder={`New ${keyLabel} name`} value={newKey} onChange={(e) => setNewKey(e.target.value)} />
        )}

        <SecretCombobox
          names={secretNames}
          value={creatingSecret ? "" : secretChoice}
          disabled={busy}
          onPick={(name) => {
            setSecretChoice(name);
            setNewSecretName("");
          }}
          onNew={(name) => {
            // An explicit new-secret choice: the name (typed in the search box, or empty) becomes the pair
            // the two fields below fill in.
            setSecretChoice(NEW_SELECTION);
            setNewSecretName(name);
            setNewSecretValue("");
          }}
        />

        {creatingSecret && (
          <>
            <Input
              placeholder="New secret name (e.g. MCP_GITHUB_TOKEN)"
              value={newSecretName}
              onChange={(e) => setNewSecretName(e.target.value)}
            />
            <Input
              placeholder="Value (write-only)"
              type="password"
              value={newSecretValue}
              onChange={(e) => setNewSecretValue(e.target.value)}
            />
          </>
        )}
      </div>

      <Button
        type="button"
        size="sm"
        disabled={busy || !resolvedKey || !resolvedSecret || (creatingSecret && !newSecretValue.trim())}
        onClick={() =>
          onApply({
            targetId: target?.id ?? "",
            key: resolvedKey,
            secretName: resolvedSecret,
            createNew: creatingSecret,
            newValue: newSecretValue,
          })
        }
      >
        <KeyRound className="mr-1 h-3 w-3" /> {creatingSecret ? "Store and attach" : "Attach"}
      </Button>

      {note && <p className="text-xs text-muted-foreground">{note}</p>}
    </>
  );

  if (!target) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Credentials</CardTitle>
          <CardDescription>{description}</CardDescription>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">
            Add a server first — a credential is attached to one of this scope&apos;s servers.
          </p>
        </CardContent>
      </Card>
    );
  }

  if (compact) {
    return (
      <div className="space-y-3 rounded-md border border-white/10 p-3">
        <div className="text-sm font-medium">Credentials</div>
        <p className="text-xs text-muted-foreground">{description}</p>
        {controls}
      </div>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Credentials</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">{controls}</CardContent>
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
  const prefill = usePrefillMCPCatalogEntry();

  const [form, setForm] = useState<FormState>(emptyForm);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  // THE CREDENTIAL CONTROL OWNS ITS OWN FIELD STATE (CredentialCard below). It used to live here, in two
  // near-identical sets of fields — one for the owned scope (a server id, a typed key, a typed value) and
  // one for the inline scope (a spec, a typed key, a typed name, a typed value). Collapsing them into one
  // component is what removed the typed value: there is no field left for one.

  // The write-only credential store, read for its NAMES: the picker offers what exists (the only shape
  // a reference can take and still resolve), and the value path needs the id when it replaces one.
  const { data: secrets = [], error: secretsError } = useSecretList();
  const createSecret = useCreateSecret();
  const updateSecret = useUpdateSecret();

  // The one honest note about the store the picker reads: unreadable, or empty. Either way the operator
  // needs to know why the list is short — and "Store a new secret…" is always offered, so an unreadable
  // store never blocks a valid credential.
  const secretsNote =
    secretsError != null
      ? `The tenant secrets store could not be read (${String(secretsError)}), so nothing is offered to pick — store a new secret to proceed.`
      : secrets.length === 0
        ? 'No secrets are stored yet — choose "Store a new secret…" to add one, or create it in Settings → Secrets.'
        : undefined;

  // rows is the common render list, whatever the persistence mode.
  const rows: Row[] =
    scope.kind === "workerVersion"
      ? scope.value.map(inlineRow)
      : listed.map(ownedRow);

  // THE INLINE SPECS THEMSELVES (worker-version scope only). The credential control acts on a SPEC
  // rather than on a display row: a row's `command` is a single string with `args` beside it, while the
  // spec carries the argv and the env/headers a reference has to land in.
  const inlineSpecs: InlineMCP[] = scope.kind === "workerVersion" ? scope.value : [];

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
          // Sent, so the plane records the provenance: required_secrets, the Install control and the
          // secrets-stored badge are all derived from it. Omitting it (as this panel used to) made a
          // catalog-added server indistinguishable from a hand-written one.
          catalogSlug: form.catalogSlug,
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
      // The provenance survives an edit, so the credential control keeps offering the key this entry
      // DECLARES and the Install / secrets-stored affordances keep working.
      catalogSlug: r.catalogSlug,
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
        // THE SLUG IS REMEMBERED, not discarded. It is what makes the entry's DECLARED credential key the
        // default in the picker below, and what the create sends so the plane can derive the row's
        // required_secrets (and with them the Install control and the secrets-stored badge).
        catalogSlug: slug,
      });
      setEditingId(null);
      setShowForm(true);
    } catch (e) {
      setActionError(String(e));
    }
  }

  // handleApplyCredential attaches a STORED credential to one server, in either placement.
  //
  // ONE HANDLER, TWO TARGETS, one rule for both: the VALUE is never here. An existing secret is referenced
  // as ${NAME}; a NEW one is stored FIRST, because a reference is resolved when a session starts
  // (mcpsettings.ResolveSecretRefs) and a dangling one fails that server's session rather than this click.
  // So the order is store-then-point, at every scope.
  async function handleApplyCredential(draft: CredentialDraft) {
    setActionError(null);
    try {
      if (draft.createNew) {
        // A name the store already holds is a ROTATION, not a duplicate: the value is replaced under the
        // same name, so every server referencing it picks the new value up at once. A new name is created.
        const existing = secrets.find((s) => s.name === draft.secretName);
        if (existing) {
          await updateSecret.mutateAsync({ id: existing.id, value: draft.newValue ?? "" });
        } else {
          await createSecret.mutateAsync({
            name: draft.secretName,
            value: draft.newValue ?? "",
            description: `MCP server credential (${draft.key})`,
          });
        }
      } else if (!secretsError && !secrets.some((s) => s.name === draft.secretName)) {
        // The form's own rule, about the list it just offered: a name the store does not hold cannot be
        // referenced. Skipped when the store could not be read, so a store we cannot see never blocks a
        // name that may well be there.
        setActionError(`${draft.secretName} is not in the tenant secrets store — pick one from the list, or store a new one.`);
        return;
      }

      if (scope.kind === "workerVersion") {
        // Inline: the reference goes into the VERSION's own spec, kept by the worker form's Save.
        writeInline(attachSecret(scope.value, draft.targetId, draft.key, draft.secretName));
        return;
      }

      // THE SERVER BEING EDITED: nothing is persisted yet, so the reference is written into the FORM's own
      // env/headers text and travels with the Create/Save that follows. This is the placement the operator
      // asked for — the credential is selectable where the server is being defined, not only afterwards.
      if (draft.targetId === FORM_TARGET_ID) {
        const isHTTP = form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP;
        const next = withReference(
          parseKeyValue(form.env),
          parseKeyValue(form.headers),
          isHTTP,
          draft.key,
          draft.secretName,
        );
        setForm({ ...form, env: keyValueText(next.env), headers: keyValueText(next.headers) });
        return;
      }

      // Owned: the reference goes into the ROW's env/headers, through the MCP service.
      const row = rows.find((r) => r.id === draft.targetId);
      if (!row) {
        setActionError("Pick a server to attach the credential to.");
        return;
      }
      const isHTTP = row.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP;
      const { env, headers } = withReference(row.env, row.headers, isHTTP, draft.key, draft.secretName);
      await updateServer.mutateAsync({
        id: row.id,
        // The update REPLACES args/env/headers, so everything the row already holds is echoed back: an
        // update that sent only the credential would erase the server's own configuration.
        command: row.command ?? "",
        replaceArgs: true,
        args: row.args ?? [],
        env,
        replaceEnv: true,
        url: row.url ?? "",
        headers,
        replaceHeaders: true,
        enabled: row.enabled,
        // The owner echo is required on update — a differing echo is rejected.
        projectId: scope.kind === "project" ? scope.projectId : "",
        conversationId: scope.kind === "conversation" ? scope.conversationId : "",
      });
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
        // RAISED, so the credential control's picker list is not painted over.
        //
        // THE DEFECT: `glass-panel` carries a backdrop-filter, and a backdrop-filter creates a STACKING
        // CONTEXT. This card and the "Configured servers" card below it are SIBLINGS, both contexts, both
        // z-index auto — so the LATER one paints on top, and the dropdown's own z-50 cannot lift it: an
        // inner z-index only orders elements within its own context. The operator saw the list cut off
        // where the next card began. Giving this card an explicit position + z-index orders the two
        // siblings, and everything inside (the dropdown included) comes with it.
        <Card className="relative z-20">
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
                  placeholder="Env (KEY=VALUE per line — non-secret values; set credentials below)"
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
                  placeholder="Headers (KEY=VALUE per line — non-secret values; set credentials below)"
                  value={form.headers}
                  onChange={(e) => setForm({ ...form, headers: e.target.value })}
                />
              </>
            )}
            {/* THE CREDENTIAL CONTROL, IN THE FORM ITSELF. This is the edit page the operator meant: "we
             *  need the ability to select/create the secret credentials from the MCP edit page". Before
             *  this the only credential affordance was the Credentials card BELOW the server list — which
             *  does not exist yet while the server is being defined, so the env field was the only place
             *  left to put a credential, and it took one typed by hand.
             *
             *  The target is the FORM, so the reference lands in its env/headers text and travels with the
             *  Create/Save that follows. A new secret is still STORED FIRST (see handleApplyCredential),
             *  because a reference is resolved when a session uses the server. */}
            {!readOnly && (
              <CredentialCard
                compact
                targets={[
                  {
                    id: FORM_TARGET_ID,
                    label: form.name.trim() || "this server",
                    isHTTP: form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP,
                    env: parseKeyValue(form.env),
                    headers: parseKeyValue(form.headers),
                    // What the picked registry entry DECLARES it needs, offered before the form's own keys
                    // so the right one is the default rather than a retype.
                    declaredKeys: catalog.find((c) => c.slug === form.catalogSlug)?.requiredEnv ?? [],
                  },
                ]}
                secretNames={secrets.map((s) => s.name)}
                busy={createSecret.isPending || updateSecret.isPending}
                onApply={handleApplyCredential}
                description={
                  "Search the tenant secrets store and pick one (or create a new secret), and it is written " +
                  "into this server's env (stdio) or headers (streamable HTTP) as a ${SECRET_NAME} reference."
                }
                note={secretsNote}
              />
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

      {/* A CREDENTIAL IS SELECTED FROM THE STORE — AT EVERY SCOPE.
       *
       *  The operator's rule: "we should not allow [a typed key/value pair] at all. ALL SECRETS should be
       *  driven from the secrets that can be stored on the platform", and, for the one case no list can
       *  answer, "if the user wants to type in a NEW one to create one that is fine, otherwise it should be
       *  static". So the control below is a PICKER: the KEY comes from what the server declares or already
       *  carries, the SECRET from the store's own names, and a text field appears only where the operator
       *  chose to add something new.
       *
       *  It replaces a form whose env field invited KEY=VALUE by hand and advertised ${SECRET_NAME} as
       *  something to type — the shape being removed. A typed value in a server's config is a credential in
       *  the wrong place: not rotated with the store, not write-only, and written into the version's
       *  permissions JSON, which is what a published version is immutable ABOUT.
       *
       *  ONE CONTROL, THREE SCOPES; only the target differs. A worker version has no definition row, so the
       *  reference is written into the version's own spec; an owned row's env/headers are updated through
       *  the MCP service (the card below). Both are the same ${NAME} reference, resolved at session time by
       *  mcpsettings.ResolveSecretRefs.
       *
       *  A worker version's change is kept by the HOST FORM's Save (routes/workers_.$id.tsx), not by this
       *  panel — stated here rather than assumed, because this panel holds no write at that scope. */}
      {!readOnly && !owned && inlineSpecs.length > 0 && (
        <CredentialCard
          targets={inlineSpecs.map((sp) => ({
            id: sp.id,
            label: sp.id,
            isHTTP: inlineSpecIsHTTP(sp),
            env: sp.env,
            headers: sp.headers,
          }))}
          secretNames={secrets.map((s) => s.name)}
          busy={createSecret.isPending || updateSecret.isPending}
          onApply={handleApplyCredential}
          description={
            "Point one of this version's servers at a stored secret: the chosen name is written into that " +
            "server's env (stdio) or headers (streamable HTTP) as a ${SECRET_NAME} reference, resolved when " +
            "the worker runs. A version has no definition row, so the reference IS the credential — save the " +
            "version to keep it."
          }
          note={secretsNote}
        />
      )}

      {/* The OWNED scopes' credential: the reference lands in the ROW's env/headers, and the value is
       *  stored once in the tenant secrets store (internal/mcpsettings.ResolveSecretRefs resolves it at
       *  session time).
       *
       *  This replaces a form that typed a key and a value straight into the server — the second report:
       *  "on projects in the GUI, it does add the servers but it does NOT allow you to select/add from the
       *  secrets vault. It is still manually typed in key, value pair. We should not allow that option at
       *  all." The option is gone: there is no value field outside the "store a new secret" case, and that
       *  value goes to the STORE, never into the server. */}
      {!readOnly && owned && rows.length > 0 && (
        <CredentialCard
          targets={rows.map((r) => ({
            id: r.id,
            label: r.name,
            isHTTP: r.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STREAMABLE_HTTP,
            env: r.env,
            headers: r.headers,
            // The keys the catalog entry DECLARES this server reads, offered before its own keys — which
            // makes the right one the default for a catalog-added server instead of a retype.
            declaredKeys: catalog.find((c) => c.slug === r.catalogSlug)?.requiredEnv ?? [],
          }))}
          secretNames={secrets.map((s) => s.name)}
          busy={createSecret.isPending || updateServer.isPending}
          onApply={handleApplyCredential}
          description={
            "Point a server at a stored secret: the chosen name is written into that server's env (stdio) or " +
            "headers (streamable HTTP) as a ${SECRET_NAME} reference. The value itself is never stored on the " +
            "server — it lives once in the tenant secrets store and is resolved when a session uses it."
          }
          note={secretsNote}
        />
      )}
    </div>
  );
}
