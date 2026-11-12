# Manual evidence sheet — windows

This harness cannot install or drive a windows machine from here. Every step below is
recorded SKIP until a human attaches the screenshots. Do NOT mark them PASS.

Run, on a clean Windows 10 machine with NO plane running:

1. Install the packaged app from the `.exe` (WebView2 Evergreen bootstrapper path —
   `deploy/desktop/windows/README-WINDOWS.txt`). On a Windows 10 host with WebView2
   ABSENT, capture the dialog: a blank window is a FAIL, a precise instruction is a PASS.
2. Launch from *Apps & features* (per-user install), NOT a dev build.
3. Offline state: screenshot the panel; it must name the URL it tried.
4. Click Start; wait for the UI. `curl http://127.0.0.1:<port>/healthz` must answer.
5. Start a workflow; screenshot the live view at t0 / t0+10s / t0+20s — it must
   keep updating, not freeze.
6. Open Projects and Work Items.
7. Stop from the app: the plane must go down (Task Manager / `curl` fails), then Start.

Attach the screenshots to the work item and paste the `curl` output verbatim (AC9).
