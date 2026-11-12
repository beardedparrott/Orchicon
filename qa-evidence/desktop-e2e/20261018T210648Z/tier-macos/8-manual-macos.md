# Manual evidence sheet — macos

This harness cannot install or drive a macos machine from here. Every step below is
recorded SKIP until a human attaches the screenshots. Do NOT mark them PASS.

Run, on a clean macOS machine with NO plane running:

1. Install `/Applications/Orchicon.app` from the `.dmg`; open it (no right-click
   ceremony, or a precise Gatekeeper instruction — `deploy/desktop/macos/sign-and-notarize.sh`).
2. With no plane running, screenshot the offline state (must name the URL).
3. Click Start; UI hands over; `curl http://127.0.0.1:<port>/healthz` answers.
4. Start a workflow; screenshots at t0 / t0+10s / t0+20s must differ.
5. Projects / Work Items load.
6. Stop then Start from the app, each verified with `curl` and `pgrep`.

Attach the screenshots to the work item and paste the `curl` output verbatim (AC9).
