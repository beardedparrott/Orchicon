# CloudFlare setup for orchicon.dev

> One-time setup that points `orchicon.dev` at the GitHub Pages site
> and redirects `/install` and `/install.ps1` to the canonical install
> scripts in the repo. After this is done, the install one-liners
> `curl -fsSL https://orchicon.dev/install | bash` and
> `irm https://orchicon.dev/install.ps1 | iex` work for everyone.

## What you have today

| Piece | Where it lives | Status |
|---|---|---|
| Static site | `docs-site/` (this repo) | built, ready to deploy |
| GitHub Pages workflow | `.github/workflows/pages.yml` | ready, deploys on push to `main` |
| Custom domain | `docs-site/CNAME` (`orchicon.dev`) | ready |
| Linux/macOS installer | `scripts/install.sh` | ready (tracks `main`) |
| Windows installer | `scripts/install.ps1` | ready (tracks `main`) |

The flow:

```
orchicon.dev/                 → GitHub Pages (docs-site/index.html)
orchicon.dev/install          → 301 → raw.githubusercontent.com/.../scripts/install.sh
orchicon.dev/install.ps1      → 301 → raw.githubusercontent.com/.../scripts/install.ps1
```

Single source of truth for the installers stays in `scripts/`. The
redirect pulls from the raw GitHub URL on every install, so script
edits go live as soon as they merge.

---

## Step 1 — Enable GitHub Pages

1. In the repo on GitHub: **Settings → Pages**.
2. **Source**: *GitHub Actions* (not "Deploy from a branch"). The
   workflow at `.github/workflows/pages.yml` does the deploy.
3. Save. The first deploy will fail until DNS is set up (next step),
   but the workflow will appear under the Actions tab.
4. (Optional) **Custom domain** field will populate from `docs-site/CNAME`
   on the next successful deploy.

## Step 2 — DNS in CloudFlare

In the CloudFlare dashboard for `orchicon.dev`:

### Apex (`orchicon.dev`)

Add four A records pointing to GitHub Pages' published IPs. **Turn the
proxy OFF (DNS only — grey cloud)** for these records so the cert
provisioning + GitHub custom-domain flow works cleanly:

| Type | Name | Content | Proxy | TTL |
|---|---|---|---|---|
| A | `@` | `185.199.108.153` | DNS only | Auto |
| A | `@` | `185.199.109.153` | DNS only | Auto |
| A | `@` | `185.199.110.153` | DNS only | Auto |
| A | `@` | `185.199.111.153` | DNS only | Auto |

> **Why DNS-only?** GitHub Pages' custom-domain + Let's Encrypt
> certificate flow expects to see traffic from the GitHub IPs and
> issue the cert itself. CloudFlare's proxy (orange cloud) sits in
> front and breaks the ACME challenge. Once the cert is issued, you
> *can* re-enable the proxy, but for a low-traffic landing page
> DNS-only is simpler and faster.

### `www` (`www.orchicon.dev`)

Add a CNAME record:

| Type | Name | Target | Proxy |
|---|---|---|---|
| CNAME | `www` | `beardedparrott.github.io` | DNS only |

This makes `www.orchicon.dev` resolve to the same GitHub Pages site.
The apex → www redirect (or vice versa) is handled in Step 3.

## Step 3 — Redirects in CloudFlare

In **Rules → Redirect Rules** (the modern, free successor to Page
Rules — it replaced Page Rules on the free tier in 2024):

### 3a. Apex → `www` (canonical)

| Field | Value |
|---|---|
| Rule name | `apex-to-www` |
| When | Hostname `equals` `orchicon.dev` |
| Then | URL redirect, Static, `301`, `https://www.orchicon.dev/` |

(Or do the opposite — `www` → apex — whichever you prefer. Pick one
as canonical and the other redirects. Apex is shorter and more
memorable, but `www` keeps cookies / DNS records cleaner. This doc
assumes apex → `www`.)

### 3b. `/install` → install script (Linux/macOS)

| Field | Value |
|---|---|
| Rule name | `install-bash` |
| When | URI Path `equals` `/install` |
| Then | URL redirect, Static, `301`, `https://raw.githubusercontent.com/beardedparrott/Orchicon/main/scripts/install.sh` |

### 3c. `/install.ps1` → install script (Windows)

| Field | Value |
|---|---|
| Rule name | `install-ps1` |
| When | URI Path `equals` `/install.ps1` |
| Then | URL redirect, Static, `301`, `https://raw.githubusercontent.com/beardedparrott/Orchicon/main/scripts/install.ps1` |

> **Why `main` and not a tag?** The installers auto-resolve the
> latest GitHub release at runtime, so the redirect target is just
> the script body. Tracking `main` means script fixes ship without
> waiting for a release tag. Once you cut tagged releases, switch
> the redirect targets to `/vX.Y.Z/scripts/install.sh` for
> reproducibility.

### Order matters

The three rules need to evaluate before the GitHub Pages default
("serve the site"). CloudFlare Redirect Rules are evaluated in
priority order — put them at the top.

## Step 4 — Enable HTTPS on GitHub Pages

Back in the repo on GitHub: **Settings → Pages**.

After the DNS A records propagate (5–30 min):

1. Wait until the `Pages` workflow on `main` succeeds.
2. Re-check **Settings → Pages → Custom domain**. It should now show
   `orchicon.dev` as a green check.
3. Tick **Enforce HTTPS**. GitHub issues a Let's Encrypt cert via
   the ACME challenge; this can take a few minutes.

If "Enforce HTTPS" stays greyed out, the cert is still provisioning
or the DNS is not yet pointing at GitHub. Wait and retry.

## Step 5 — Verify

End-to-end checks once everything has settled:

```bash
# Page renders
curl -I https://orchicon.dev/                              # → 200, content-type: text/html
curl -I https://www.orchicon.dev/                          # → 301 → orchicon.dev/ (or vice versa)

# Install redirects
curl -fsSL -o /dev/null -w '%{http_code} %{url_effective}\n' https://orchicon.dev/install
# expected: 301 https://raw.githubusercontent.com/beardedparrott/Orchicon/main/scripts/install.sh

curl -fsSL -o /dev/null -w '%{http_code} %{url_effective}\n' https://orchicon.dev/install.ps1
# expected: 301 https://raw.githubusercontent.com/beardedparrott/Orchicon/main/scripts/install.ps1

# Install actually runs
curl -fsSL https://orchicon.dev/install | bash -- --dry-run
# expected: prints the steps it would take, exits cleanly without installing
```

And the same for PowerShell on Windows:

```powershell
irm https://orchicon.dev/install.ps1 -OutFile install.ps1; .\install.ps1 -DryRun
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Pages workflow deploys to a `*.github.io` URL, not `orchicon.dev` | The `CNAME` file isn't being picked up | Confirm `docs-site/CNAME` exists with content `orchicon.dev` (no trailing newline issues); re-run the workflow |
| Custom domain stuck on "Unavailable" | DNS A records not yet propagated | Wait, or `dig orchicon.dev A` to confirm the GitHub IPs |
| "Enforce HTTPS" greyed out | Cert not yet provisioned, or A records still proxying (orange cloud) | Turn the orange cloud OFF on the A records |
| `/install` returns 404 instead of redirecting | Redirect rule not yet propagated, or order wrong | In CloudFlare Rules, drag the install rules to the top of the list |
| `curl \| bash` downloads but the script is stale | Browser/CDN cache, or a tagged-version redirect was used | The redirect goes to `…/main/scripts/install.sh` — if you changed it to a tag, the redirect itself needs updating when you cut a new release |
| CloudFlare shows "Orange cloud" warning when re-enabling proxy | ACME cert renewal depends on the GitHub IPs | Either keep proxy OFF permanently, or set up CloudFlare Origin CA + upload cert to GitHub Pages (advanced, not needed for a landing page) |

---

## What to update when the project evolves

| Change | Where to update |
|---|---|
| New installer flag or message | `scripts/install.sh` / `scripts/install.ps1` only — no DNS/redirect change needed (they redirect to the raw files) |
| Cut a tagged release (e.g. `v0.2.0`) | Optional: change the two `/install*` redirect targets in CloudFlare from `…/main/…` to `…/v0.2.0/…` for reproducibility |
| Page copy / design | Edit `docs-site/index.html` and `docs-site/style.css`, push to `main` — the Pages workflow auto-deploys |
| Add a new path the page should serve (e.g. `/changelog`) | Add a file under `docs-site/` and push |
| Drop the `www` redirect | Disable the `apex-to-www` rule in CloudFlare |
| Move off GitHub Pages | Replace the `CNAME` and the GitHub Pages workflow; keep the CloudFlare redirects pointing at the new origin |
