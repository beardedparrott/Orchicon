// The credential half of an INLINE MCP spec (a worker version's), as pure functions.
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
