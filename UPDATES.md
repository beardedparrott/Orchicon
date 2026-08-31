# UPDATES.md

> Track record of what has been shipped, phase-by-phase.
>
> **Lifecycle:** one row per PR, appended to the top (newest first). Row
> numbers are MONOTONIC — never renumber, the release-notes boundary derives
> from the previous release's copy. Rows up to and including the last
> release are trimmed by `scripts/gen-release-notes.sh --trim` on the first
> develop merge after a release. The GitHub Release body is NOT generated
> from these rows — it comes from RELEASE_HIGHLIGHTS.md (curated narrative)
> via scripts/gen-release-notes.sh; these rows are the internal
> engineering track record.

| # | Type | Phase | Summary |
|---|---|---|---|
| 253 | Chore | Release/Site | **Post-release housekeeping for v0.2.0: UPDATES trim + site footer version.** UPDATES.md trimmed of every released row (208–252 dropped, numbering untouched — the boundary derives from the previous release's copy of this file, so this is safe); the file now carries the next release cycle only. `site/index.html` footer version bumped v0.1.293 → v0.2.0 (the prep PR bumped the "Latest release" bar but missed the footer); `scripts/install.ps1` usage-comment example bumped v0.1.173 → v0.2.0 to match install.sh. Rides the first develop merge after the release per the trim contract, then a bare develop→main merge (NO release label — main's tree gets the site fix, Cloudflare Pages rebuilds, and auto-release.yml stays idle because the label gate doesn't match). The v0.2.0 GitHub Release itself published clean on the tag-push run (all 6 platform assets + GHCR images; the first workflow_dispatch attempt failed on a transient `npm ci` and was superseded). |