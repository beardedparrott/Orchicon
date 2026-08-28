# Native Secrets Store — Architecture Summary

**Work item:** Native secrets store (encrypted at rest, selectable per work item, Settings manager)  
**Branch:** `native-secrets-store-encrypted-at-rest-selectable-per-work-item-settings-manager-gs2dr8waxy3pqf79`  
**Date:** 2026-08-28  
**Author:** Principal Software Architect (Step 1/7)  
**Status:** Approved for implementation — Design-Approver review next

> Delivery delta for downstream workers. File lives at `architecture-notes/native-secrets-store-encrypted-at-rest-selectable-per-work-item-settings-manager.md`.

---

## 1. Context & Problem

API keys (Tavily etc.) live in unsecured text files for Automation Research / Feedback Miner. Need tenant-scoped first-class secrets store: encryption at rest (KEK), selectable injection per work item (regular AND recurring), Settings UI, MCP, audit, daemon-time env injection mirroring GH_TOKEN (internal/runtime/daemon.go). Plaintext never persisted, never baked into images, never in audit/outbox, never cross-tenant.

Constraints: single tenant per deployment (app.tenant_id RLS), forward-only Atlas migrations, audit transactional outbox, daemon is sole Docker socket holder, work_items is single table (recurring = flag), proto under proto/orchicon/api/v1, MCP registry internal/askorchicon/tools.go must stay synced.

---

## 2. Decisions (ADR: Context -> Decision -> Consequences)

### ADR-1 — New entity tenant_secrets, not tenant_settings blob
**Context:** Extend tenant_settings JSONB vs new table.  
**Decision:** New table tenant_secrets + proto TenantSecret + SecretsService. Analogous to workers/projects.  
**Consequences:** Clean RBAC, audit, pagination. Cost: one migration + service (justified).

### ADR-2 — Encryption: app-layer AES-256-GCM + KEK (v1), not pgcrypto nor external KMS
**Decision:** App-layer AES-256-GCM. KEK = 32 bytes from ORCHICON_SECRETS_KEK_B64 (base64). Ciphertext stored as v1:<key_version>:<base64(nonce||ciphertext||tag)>, nonce 12B rand, key_version int. Encrypt in internal/secrets/crypto.go. DB column ciphertext TEXT NOT NULL + key_version. KEK only in plane. Interface KMSEncryptor so KMS can replace env KEK later without schema change.  
**Trade-offs:** pgcrypto puts KEK in DB session; external KMS correct long-term but heavy for v1.

### ADR-3 — Per-work-item selection: work_items.secret_ids JSONB (covers regular & recurring)
**Decision:** ADD COLUMN work_items.secret_ids JSONB NOT NULL DEFAULT '[]' (array of secret ULIDs). Validated at API boundary (exists, same tenant, name regex ^[A-Z][A-Z0-9_]+$). Recurring fires inherit parent secret_ids at dispatch time (no duplication). Dangling ref -> dispatch fails FailedPrecondition. Cap 10 per item.  
**Alternative:** join table gives FK but adds complexity; defer, can migrate later without breaking proto.

### ADR-4 — Proto & service
- proto secret.proto: TenantSecret {id, tenant_id, name, description, created_at, updated_at} never includes value. CreateSecretRequest {name, value, description}, UpdateSecretRequest {id, value?, description?}. List/Get return metadata only (write-only vault).
- secret_service.proto: SecretsService {List,Get,Create,Update,Delete}
- work_item.proto: repeated string secret_ids = 39; propagate to CreateWorkItemRequest/UpdateWorkItemRequest
- make gen required

### ADR-5 — Runtime injection: plane decrypts, daemon injects (mirrors GH_TOKEN)
**Flow:** Reconciler -> secrets.ResolveForWorkItem(ctx, tx, tenantID, ids) -> []SecretEnv{Name, Plaintext} (decrypt via KEK in-memory) -> runtime.CreateRequest{Secrets: []SecretEnv} -> daemon createContainer appends "-e NAME=VALUE" (same loop as GH_TOKEN), logs only names. Secrets not in hostInputsFingerprint (per-run, not host input); warm pool reset already gives fresh container.
**Rejected:** daemon-side decrypt (expands TCB, needs DB+KEK in daemon).

### ADR-6 — Settings UI & MCP
**Frontend:** Settings -> Secrets section (table name/description/updated_at, create/edit/delete dialogs, password field masked, write-only). Work item create/edit/detail + recurring-items get reusable Secrets picker (multi-select from ListSecrets, pills show names).  
**MCP:** list_secrets, get_secret, create_secret, update_secret, delete_secret in internal/askorchicon/tools.go + tool_secrets.go.

### ADR-7 — Audit & log hygiene
Audit tenant_secret.created/updated/deleted, snapshots contain only id/name/description/key_version (never value/ciphertext). RLS tenant_isolation ENABLE+FORCE. Never log plaintext/ciphertext/KEK.

---

## 3. Data Model

### tenant_secrets
```sql
CREATE TABLE tenant_secrets (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id),
  name TEXT NOT NULL CHECK (name ~ '^[A-Z][A-Z0-9_]+$'),
  description TEXT NOT NULL DEFAULT '',
  ciphertext TEXT NOT NULL,
  key_version SMALLINT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);
CREATE INDEX tenant_secrets_tenant_id_idx ON tenant_secrets(tenant_id);
ALTER TABLE tenant_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_secrets USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
```

### work_items.secret_ids
```sql
ALTER TABLE work_items ADD COLUMN secret_ids JSONB NOT NULL DEFAULT '[]';
```

Update db/schema.hcl declarative source; forward-only migrations rehash atlas.sum.

---

## 4. Encryption at Rest
- internal/secrets/crypto.go: Encrypt/Decrypt using crypto/aes cipher.NewGCM, nonce 12B rand, format v1:<ver>:<b64>. KEK from config (ORCHICON_SECRETS_KEK_B64 32B after decode), Validate fails closed if missing/invalid.
- Rotation: new KEK version additive; old rows keep old version; map[version]kek for decrypt; re-encrypt job out of scope v1.
- Verification: DB test raw SELECT ciphertext != plaintext and Decrypt round-trips.

---

## 5. Per-Item Selection (unified)
Regular and recurring share same column (recurring is flag). Scheduler due-scan loads secret_ids and passes to CreateWorkflowRun dispatch path; secrets resolved at dispatch (edits apply to next fire). Dangling secret -> scheduled->failed with secret not found, audited.

---

## 6. Daemon Injection
```go
type SecretEnv struct { Name string `json:"name"`; Value string `json:"value"` }
type CreateRequest struct { ... Secrets []SecretEnv `json:"secrets,omitempty"` }
// createContainer after GH_TOKEN:
for _, s := range req.Secrets { args = append(args, "-e", s.Name+"="+s.Value) }
```
Plane builds Secrets by decrypting; daemon blind inject, never logs values. Pool key unchanged.

---

## 7. Implementation Plan (ordered, minimal delta)
A Proto+DB: secret.proto, secret_service.proto, work_item.proto secret_ids, schema.hcl, migrations, make gen
B Service+crypto: internal/secrets/crypto.go, service.go, db/secrets.go, config SecretsKEK, db/work_item.go secret_ids, workitem/service.go validation
C Runtime: daemon.go SecretEnv+CreateRequest.Secrets+createContainer loop, scheduler thread-through
D UI: lib/api/secrets.ts, hooks/useSecrets.ts, settings.secrets.tsx or settings.tsx section, work-item-secrets-picker.tsx, integrate work-items & recurring-items routes
E MCP+audit: askorchicon/tool_secrets.go + tools.go, audit redaction
F Tests: secrets/service_test.go (RLS, ciphertext!=plaintext), daemon injection test, audit redaction, frontend Playwright, recurring inherit test
Plus docs/09, docs/02, DOCUMENTATION.md updates.

---

## 8. Scale / 10x
Secrets per tenant 10k paginated; per-item dispatch single WHERE id=ANY($1) (<=10 ids); GIN not needed. Secrets not in hostInputsFingerprint so pool unaffected. Env size bounded 80KiB. Per-tenant KEK derivation future (HKDF tenant_id) if multi-tenant-per-plane.

## 9. Risks
KEK in logs -> Validate rejects example, never log. Plaintext in run_context -> excluded. Env arg size -> cap 10. Uniqueness error -> generic AlreadyExists.

## 10. Acceptance Mapping
proto+migration+service+audit §3/§7; encryption §4; per-item regular+recurring §5; daemon env §6; Settings UI §6; tests §7F.

## 11. References
daemon.go GH_TOKEN, pool.go hostInputsFingerprint, audit/audit.go, auth/op/keys.go, db/schema.hcl, work_item.proto, askorchicon/tools.go, settings.tsx

*Downstream: Design Approver validates KEK provisioning + Reveal RPC question; Senior Engineer implements per §7 order.*

