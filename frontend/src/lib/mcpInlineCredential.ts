// The credential half of an MCP server's env/headers, as pure functions — BOTH placements.
//
// It began as the inline (worker-version) half and now serves an OWNED row as well, because the operator's
// rule turned out to make the two identical: a credential is DRAWN FROM THE STORE, never typed into the
// server, and the thing that gets stored on the server is the reference `${SECRET_NAME}` — which
// internal/mcpsettings.ResolveSecretRefs resolves at session time for an owned row and an inline spec
// alike (internal/server/server.go, internal/runtime/lifecycle.go). So one control serves all three
// scopes, differing only in WHERE the reference lands: a worker version's permissions JSON, or an owned
// row's env/headers.
//
// SELECT FROM THE STORE, TYPE ONLY TO CREATE. A value is never handled here. The KEY and the NAME are
// both STATIC where a static answer exists — the keys the server already declares and carries
// (credentialKeyCandidates), and the names the store already holds — and typing is offered only to
// introduce something new, which is the one case no list can answer. The write stays a pure
// `${NAME}`-into-a-map either way, so the new/static distinction lives in the form, not here.
//
// WHY THIS IS A LIB AND NOT INLINE IN THE PANEL. A worker version has no definition row, so a credential
// cannot be STORED against it — it has to be REFERENCED, and the reference is built into the spec's own
// env (stdio) or headers (streamable HTTP). That is exactly the sentence the operator asked for: "the
// credentials should be able to be selected rather than typed just like it does for projects and
// conversations. I understand that it is inline but that doesn't mean that info can't be built on top
// and passed to the config."
//
// It is also the ONE place the decision "which map does this spec read?" is made, so the picker's
// suggestions and the write cannot disagree — and it mirrors the TUI's own
// internal/tui/screens/mcpforms.AttachSecret, which does the same thing for the terminal client.
//
// Values are never handled here: a credential is a ${SECRET_NAME} reference, resolved plane-side at
// session time (internal/mcpsettings.ResolveSecretRefs).

// InlineSpecLike is the part of an inline spec a credential reference touches — structurally satisfied
// by the panel's InlineMCP, so this lib needs no import from a component.
export interface InlineSpecLike {
  id: string;
  type?: string;
  url?: string;
  command?: string[];
  env?: Record<string, string>;
  headers?: Record<string, string>;
}

// inlineSpecIsHTTP reports whether an inline spec speaks streamable HTTP, mirroring the server's own
// inference (internal/mcpsettings.specFromInline: the type first, the url as the fallback) so the map
// written here is the map the spec's transport actually reads.
export function inlineSpecIsHTTP(spec: InlineSpecLike): boolean {
  if (spec.type === "http" || spec.type === "streamable-http") return true;
  if (spec.type === "stdio") return false;
  return !!spec.url;
}

// credentialKeys lists the keys a spec already carries for its transport — what a credential picker
// should offer, so the common case (point this server's existing GITHUB_TOKEN at a stored secret) is a
// pick rather than a retype. An empty list is normal: a catalog pick deliberately leaves a secret key
// out, and the field stays typable for exactly that case.
export function credentialKeys(spec: InlineSpecLike | undefined): string[] {
  if (!spec) return [];
  const m = inlineSpecIsHTTP(spec) ? spec.headers : spec.env;
  return Object.keys(m ?? {}).sort();
}

// credentialKeyCandidates lists the keys a credential could fill for a server, in the order a picker
// should offer them: the keys the server DECLARES it needs first (a catalog entry's requiredEnv — the
// server's own statement of which credential it reads), then whatever keys it already carries for its
// transport. Deduplicated, declaration first, trimmed.
//
// Declared-before-existing is the useful order: for a catalog entry the declared key IS the answer, and
// the already-present keys are usually the non-secret ones (PATH, LOG_LEVEL). It also means an arbitrary
// server that declares nothing still gets its own existing keys rather than an empty list.
export function credentialKeyCandidates(
  env: Record<string, string> | undefined,
  headers: Record<string, string> | undefined,
  isHTTP: boolean,
  declared: string[] = [],
): string[] {
  const existing = Object.keys((isHTTP ? headers : env) ?? {});
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of [...declared, ...existing]) {
    const k = raw.trim();
    if (k && !seen.has(k)) {
      seen.add(k);
      out.push(k);
    }
  }
  return out;
}

// referenceName reads the ${SECRET_NAME} reference a value carries, MIRRORING the plane's own
// mcpsettings.secretRefName (a "${" prefix and a "}" suffix) so the client recognises exactly the values the
// server resolves — no more and no fewer.
//
// ONE CONSEQUENCE OF MIRRORING IT: `${}` is a reference with an EMPTY name, because the plane's own check is
// length-and-affixes only (and then looks up a secret named "", which cannot exist). So this returns "" for
// it rather than pretending the grammar is stricter than it is. Callers that need a real name test for truth
// — existingReference does, so an empty reference is not reported as an attachment.
export function referenceName(value: string | undefined): string | undefined {
  if (!value) return undefined;
  const v = value.trim();
  if (v.length >= 3 && v.startsWith("${") && v.endsWith("}")) return v.slice(2, -1);
  return undefined;
}

// existingReference finds the credential a server ALREADY carries: the first key whose value is a ${NAME}
// reference, in the map its transport reads.
//
// IT EXISTS SO THE CONTROL CAN SHOW WHAT IS ALREADY ATTACHED. Without it the picker opens blank even though
// the server's env holds a reference, and the operator reads that as the credential having been lost — "when
// you save an MCP server and go back into it, it doesn't list the credential that was already
// created/added. It is blank again. It should retain that credentials."
export function existingReference(
  env: Record<string, string> | undefined,
  headers: Record<string, string> | undefined,
  isHTTP: boolean,
): { key: string; secretName: string } | undefined {
  const m = (isHTTP ? headers : env) ?? {};
  for (const [key, value] of Object.entries(m)) {
    const secretName = referenceName(value);
    if (secretName) return { key, secretName };
  }
  return undefined;
}

// withReference writes `${NAME}` at KEY in the map this transport reads, returning NEW maps — for an
// OWNED row, whose env/headers are updated through the MCP service (replaceEnv/replaceHeaders), which is
// the only difference from an inline spec's attachSecret below: same write, same reference, different
// target. Both spell their map the same way so the picker's suggestion and the write cannot disagree.
export function withReference(
  env: Record<string, string> | undefined,
  headers: Record<string, string> | undefined,
  isHTTP: boolean,
  key: string,
  secretName: string,
): { env: Record<string, string>; headers: Record<string, string> } {
  const k = key.trim();
  const name = secretName.trim();
  const e = { ...(env ?? {}) };
  const h = { ...(headers ?? {}) };
  if (!k || !name) return { env: e, headers: h };
  const ref = "${" + name + "}";
  if (isHTTP) h[k] = ref;
  else e[k] = ref;
  return { env: e, headers: h };
}

// attachSecret points one key of one spec at a stored tenant secret, as ${NAME}.
//
// It returns NEW objects (the caller owns the array through a form field, so the change has to be
// observable), replaces an existing value at that key — that is the point: a plaintext credential
// becomes a reference — and leaves every other spec and key exactly as it was.
export function attachSecret<T extends InlineSpecLike>(
  specs: T[],
  specId: string,
  key: string,
  secretName: string,
): T[] {
  const k = key.trim();
  const name = secretName.trim();
  if (!k || !name) return specs;
  const ref = "${" + name + "}";
  return specs.map((spec) => {
    if (spec.id !== specId) return spec;
    if (inlineSpecIsHTTP(spec)) {
      return { ...spec, headers: { ...(spec.headers ?? {}), [k]: ref } };
    }
    return { ...spec, env: { ...(spec.env ?? {}), [k]: ref } };
  });
}
