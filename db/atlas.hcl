# Orchicon Atlas configuration.
#
# Declarative schema lives in schema.hcl (docs/09 §8). Versioned
# migrations live in migrations/ and are forward-only (AGENTS.md
# invariant #9). The control plane applies migrations on startup under
# a cluster-wide advisory lock (docs/09 §8).
#
# Local dev uses the docker-compose Postgres (deploy/compose). Set
# ATLAS_DSN to point elsewhere.

variable "db_url" {
  type    = string
  default = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable&search_path=public"
}

env "local" {
  url = var.db_url
  dev = "docker://postgres/16/dev?search_path=public"
  migration {
    dir = "file://migrations"
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}

# Set ATLAS_DSN to override for CI/production.
env "ci" {
  url = var.db_url
  dev = "docker://postgres/16/dev?search_path=public"
  migration {
    dir = "file://migrations"
  }
}
