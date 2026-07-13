# CloudFlare setup for orchicon.dev

> One-time setup that wires `orchicon.dev` to a CloudFlare Pages
> project, gives the install one-liners
> `curl -fsSL https://orchicon.dev/install | bash` and
> `irm https://orchicon.dev/install.ps1 | iex` a working URL, and
> auto-deploys on every push to `main`.
>
> CloudFlare Pages is free for unlimited static sites with custom
> domains + automatic HTTPS. The repo stays private — the CloudFlare
> GitHub App gets scoped access to chosen repos only.

## What you have today

| Piece | Where it lives | Status |
|---|---|---|
| Static site | `site/` (this repo) | ready, `index.html` + `style.css` |
| Build config | `wrangler.toml` + `scripts/build-site.sh` | ready |
| Linux/macOS installer | `scripts/install.sh` | ready (single source of truth) |
| Windows installer | `scripts/install.ps1` | ready (single source of truth) |
| Pages project | *not yet created* | this doc walks you through it |

The flow:

```
push to main
  → CloudFlare Pages detects via the GitHub App
  → runs scripts/build-site.sh   (copies scripts/install.{sh,ps1} → site/)
  → deploys site/                (index.html, style.css, install, install.ps1)
  → served at https://orchicon-site.pages.dev (preview)
  → and https://orchicon.dev (after custom domain attaches)
```

There are **no CloudFlare redirect rules to maintain** — the install
scripts are served directly from the Pages bundle. When you edit
`scripts/install.sh` or `scripts/install.ps1` and push, the next
deploy serves the new version. Single source of truth stays in
`scripts/`.

---

## Step 1 — Create the Pages project

In the CloudFlare dashboard: **Workers & Pages → Create application →
Pages → Connect to Git**.

1. Pick the **beardedparrott/Orchicon** repo.
2. **Project name**: `orchicon-site` (this is what shows up in the
   default `*.pages.dev` URL).
3. Click **Begin setup**.

The CloudFlare GitHub App will be installed on your account with
access only to the repos you approve. It can deploy from private
repos — the world does not see the source.

## Step 2 — Build settings

In **Build settings**:

| Field | Value |
|---|---|
| Framework preset | *None* |
| Build command | `bash scripts/build-site.sh` |
| Build output directory | `site` |
| Root directory (advanced) | *(leave blank — repo root)* |
| Environment variables | none |

These mirror `wrangler.toml`. If you change them in the dashboard,
keep `wrangler.toml` in sync (and vice versa) — they are the two
sources of build config; treat the file as the source of truth for
anyone reading the repo.

Click **Save and Deploy**. The first build will run; it should
succeed and produce a `*.pages.dev` URL.

## Step 3 — Attach the custom domain

In **Custom domains → Set up a custom domain**:

1. Enter `orchicon.dev`.
2. CloudFlare will detect that the domain is already on its DNS and
   add the right CNAME automatically. Click **Activate domain**.
3. Repeat for `www.orchicon.dev` if you want the `www` subdomain to
   serve the same site.

CloudFlare provisions the certificate and attaches the domain within
a minute or two. No DNS records to add, no A-record drift, no
"Enforce HTTPS" button to wait on — Pages handles all of it.

## Step 4 — Optional: redirect apex → `www` (or vice versa)

In **Rules → Redirect Rules** (free on every plan):

| Field | Value |
|---|---|
| Rule name | `apex-to-www` |
| When | Hostname `equals` `orchicon.dev` |
| Then | URL redirect, Static, `301`, `https://www.orchicon.dev/` |

Or do `www` → apex. Pick one canonical and redirect the other.

## Step 5 — Verify

End-to-end checks once the first deploy settles:

```bash
# Page renders
curl -I https://orchicon.dev/                               # → 200, content-type: text/html

# Install scripts are served from the same origin
curl -fsI https://orchicon.dev/install                      # → 200
curl -fsI https://orchicon.dev/install.ps1                  # → 200
curl -fsSL https://orchicon.dev/install | head -5           # → shows the shebang + the first comments
curl -fsSL https://orchicon.dev/install.ps1 | head -5       # → shows the PowerShell param block

# Install actually runs (dry-run, no install)
curl -fsSL https://orchicon.dev/install | bash -s -- --dry-run
# expected: prints the steps it would take, exits cleanly
```

And on Windows:

```powershell
irm https://orchicon.dev/install.ps1 -OutFile install.ps1; .\install.ps1 -DryRun
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Build fails with `bash: scripts/build-site.sh: No such file` | Root directory set to `site/` or some subdir | Set root directory to blank (repo root) |
| Build succeeds but `orchicon.dev/install` 404s | Build output directory wrong | Set it to `site` (not `./site` and not the repo root) |
| Custom domain stuck on "Pending" | DNS not on CloudFlare | Move `orchicon.dev` nameservers to CloudFlare if it isn't already; or add the domain to CloudFlare via *Add Site* and follow the onboarding |
| `curl -fsSL https://orchicon.dev/install` downloads HTML, not the script | A `_redirects` or `_headers` file is intercepting | You don't have either in `site/`. If you add them later, make sure they don't blanket-rewrite the install paths |
| Install serves an older version than the one in `scripts/install.sh` | A previous deploy is cached, or a Pages branch override is in effect | Trigger a fresh deploy from the dashboard; check **Settings → Builds → Branch control** is on `main` |
| GitHub App won't connect to a private repo | The App wasn't granted access to the repo | CloudFlare dashboard → Settings → Builds → "Manage GitHub access" → grant the Orchicon repo |

---

## What to update when the project evolves

| Change | Where to update |
|---|---|
| New installer flag or message | `scripts/install.sh` / `scripts/install.ps1` only — `build-site.sh` picks it up automatically |
| Page copy / design | `site/index.html` + `site/style.css`, push to `main` — auto-deploys |
| Add a new path the page should serve (e.g. `/changelog`) | Add the file under `site/` and push |
| Add HTTP headers (security, caching) | Drop a `site/_headers` file — Pages picks it up |
| Add a redirect (e.g. `/docs → /docs/`) | Drop a `site/_redirects` file — Pages picks it up |
| Change the project name (default `*.pages.dev` URL) | Rename in dashboard; update `wrangler.toml` `name` field to match |
| Move off CloudFlare Pages | Replace `wrangler.toml` + `scripts/build-site.sh`; keep `site/` as the source for any other static host |
