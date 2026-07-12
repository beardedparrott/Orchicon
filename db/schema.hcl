// Orchicon declarative schema (source of truth for table structure).
//
// Mirrors docs/09_Database_Schema.md. The first revision covers the
// Identity & Tenancy and Projects table groups (docs/09 §3.1, §3.2).
//
// Row-Level Security policies are NOT expressed here because Atlas's
// free tier does not diff RLS policies. They are maintained as raw SQL
// appended to the generated migrations (see migrations/). The
// data-access layer is the primary isolation layer; RLS is the
// backstop (docs/09 §8.5). FORCE ROW LEVEL SECURITY is set so the
// policy applies to the table owner too — the control plane's DB role
// must set app.tenant_id per transaction or see no rows.

schema "public" {
}

table "tenants" {
  schema = schema.public
  comment = "Tenant root; budget envelope, default policies (docs/09 §3.1)"

  column "id" {
    type = text
    null = false
  }
  column "slug" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "active"
  }
  column "budget_envelope" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "default_policy_refs" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "tenants_slug_idx" {
    unique  = true
    columns = [column.slug]
  }
}

table "identities" {
  schema = schema.public
  comment = "Users and service accounts within a tenant (docs/09 §3.1). RLS-enabled: see migrations/."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "subject" {
    type = text
    null = false
  }
  column "display_name" {
    type = text
    null = true
  }
  column "status" {
    type = text
    null = false
    default = "active"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "identities_tenant_subject_idx" {
    unique  = true
    columns = [column.tenant_id, column.subject]
  }
}

table "projects" {
  schema = schema.public
  comment = "Top-level tenant of work state (docs/09 §3.2, docs/02 §2.1). RLS-enabled: see migrations/."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "name" {
    type = text
    null = false
  }
  column "slug" {
    type = text
    null = false
  }
  column "status" {
    type = text
    null = false
    default = "drafting"
  }
  column "goals" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "budget_envelope" {
    type = jsonb
    null = false
    default = "{}"
  }
  column "default_policy_refs" {
    type = jsonb
    null = false
    default = "[]"
  }
  column "version" {
    type = integer
    null = false
    default = 1
  }
  column "created_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "updated_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }

  primary_key {
    columns = [column.id]
  }

  index "projects_tenant_slug_idx" {
    unique  = true
    columns = [column.tenant_id, column.slug]
  }
  index "projects_tenant_status_idx" {
    columns = [column.tenant_id, column.status]
  }
}

table "outbox" {
  schema = schema.public
  comment = "Transactional outbox: every mutation writes a row here in the same tx (docs/09 §6, §3.9). RLS-enabled."

  column "id" {
    type = text
    null = false
  }
  column "tenant_id" {
    type = text
    null = false
  }
  column "event_id" {
    type = text
    null = false
  }
  column "aggregate_type" {
    type = text
    null = false
  }
  column "aggregate_id" {
    type = text
    null = false
  }
  column "aggregate_version" {
    type = integer
    null = false
  }
  column "event_type" {
    type = text
    null = false
  }
  column "payload" {
    type = jsonb
    null = false
  }
  column "occurred_at" {
    type = timestamptz
    null = false
    default = sql("now()")
  }
  column "published_at" {
    type = timestamptz
    null = true
  }
  column "trace_id" {
    type = text
    null = true
  }
  column "correlation_id" {
    type = text
    null = true
  }

  primary_key {
    columns = [column.id]
  }

  // Hot path: relay polls unpublished rows ordered by occurrence time
  // (docs/09 §7). Partial index keeps it small as rows get published.
  index "outbox_unpublished_idx" {
    columns = [column.occurred_at]
    where = "published_at IS NULL"
  }
  index "outbox_event_id_idx" {
    unique  = true
    columns = [column.event_id]
  }
}
