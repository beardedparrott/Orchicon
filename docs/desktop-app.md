# Desktop app (`orchicon-desktop`)

A native window around the **same** React SPA the plane serves, talking to the **same** plane.
Third packaging of existing parts — not a second client.

## The origin decision (the crux)

The shell runs its own loopback HTTP server (`http://127.0.0.1:<ephemeral>`, `internal/desktop`),
serves the **embedded** SPA from it, and reverse-proxies the plane's API paths to the configured plane.

| Rejected alternative | Why |
| --- | --- |
| `wails://` custom scheme | Not a cookie-bearing origin. Auth is a bearer token **plus an HttpOnly refresh cookie** (`frontend/src/api/clients.ts`), so the 401-refresh-retry would loop forever. |
| CORS on the plane | The plane has no CORS middleware; adding one widens what the API accepts — exactly the "weakening auth" the brief forbids. |
| Build-time/runtime base URL in the SPA | Unnecessary: the SPA calls `window.location.origin`, which under the shell **is** the shell's origin. `frontend/vite.config.ts` is untouched. |
| `file://` | No cookies, no streaming, no origin. |

Consequences, all deliberate:

- `internal/api` gained **no** route; `TestAPISurfaceDidNotGrow` still pins the procedure count.
- The shell never proxies `/` to the plane, so a plane of a different version cannot supply the UI.
- The proxy **flushes every write** (`FlushInterval: -1`) and the server has **`WriteTimeout: 0`**, so a
  Connect server-stream (a live execution's events) keeps updating. `TestProxyStreamsIncrementally`
  asserts frame 1 is readable before frame 2 exists.

## The shell, specified

| Aspect | Decision |
| --- | --- |
| **Title** | `Orchicon — <instance> (<host:port>)`, e.g. `Orchicon — dev (127.0.0.1:8080)`. Unknown port → `unknown`, raw host:port shown. |
| **Initial size** | 1440×900. |
| **Minimum size** | 1024×700 (chosen, not derived — the SPA has no responsive breakpoint; a minimum is cheaper than making it responsive). |
| **Remembered geometry** | `<os.UserConfigDir()>/orchicon-desktop/window.json` (`x,y,w,h,maximised`). Restored **only onto a visible monitor**: a rect that intersects no screen is centred on the primary with the saved size kept. |
| **Chrome** | **No native menu bar, no in-webview header — the SPA's own top bar IS the app chrome.** The shell draws zero pixels above the content, because a second header is the likeliest drift from the web app. |
| **macOS traffic lights** | The **standard native title bar** is retained (`mac.TitleBarDefault()`). Content starts below it, so nothing sits under the traffic lights and nothing can clip the SPA's top bar. A hidden-inset bar would require shell-drawn chrome — rejected for the same reason as above. |
| **Appearance** | The window **frame follows the OS** (`mac.DefaultAppearance`); the shell sets no colour, class or `data-theme`. The **SPA's theme store is the sole authority for the content area** (default dark = Teal Depths). |
| **Navigation** | The webview is loaded at the shell's own origin (the asset handler 307s there and serves nothing else). The shell intercepts no navigation: a link the SPA opens with `target="_blank"` is left to the platform webview, exactly as in a browser. |

## The desktop signal (one flag, one helper)

The shell injects exactly one global into the served `index.html`:

```js
Object.defineProperty(window, "__ORCHICON_DESKTOP__", {
  value: Object.freeze({desktop: true, planeURL, instance, appTag, planeTag, versionMismatch}),
  writable: false, configurable: false,
});
```

- It is **unforgeable from the page**: not writable, not configurable, frozen.
- The plane never injects it, so in a browser it is `undefined` (`TestPlaneNeverInjectsDesktopFlag`).
- The **only** reader is `frontend/src/lib/desktop.ts` (`desktopInfo()`, `isDesktop()`, `desktopInstance()`).
  A second detection anywhere is a bug; the Manage Server page calls `isDesktop()`.

## Instance and versions

- **Instance** comes from the plane URL's control port through `internal/controlport`, whose table is
  asserted equal to the installer's `ORCHICON_CONTROL_PORT` row (dev 8080 / prod 8091 / test 8092).
  There is no second notion of instance.
- **Version coherence**: the shell reads the plane's `/versionz` and compares it with `internal/version`.
  A mismatch (`appTag` vs `planeTag`, both known and different) is surfaced by
  `VersionMismatchBanner`, mounted above the router outlet so no route can hide it.

## Transport proof

| Layer | What proves it |
| --- | --- |
| Proxy flushes (streaming) | `TestProxyStreamsIncrementally` — unit, always runs |
| Auth headers/cookie pass through | `TestProxyForwardsAuthHeadersVerbatim` |
| Embedded SPA, never the plane's UI | `TestProxyDoesNotServeThePlaneUI` |
| Prefix table cannot drift | `TestProxyPrefixesCoverPlanePassthrough`, `TestProxyCoversEveryPlaneRoute` |
| No CORS crept in | `TestShellAddsNoCORSHeaders` |
| **Real unary + server-streaming RPCs** | `internal/desktop/e2e_live_test.go` (`-tags desktop_e2e`, needs `ORCHICON_DESKTOP_PLANE_URL`) |
| **WebKit's own fetch streaming + cookie jar** | **NOT proven here** — the E2E task's job |

## Building and running

```bash
make desktop-deps    # print host prerequisites (wails CLI, webkit2gtk, Node, CGO)
make desktop-build   # builds frontend/dist FIRST, then the app for THIS OS
make desktop-test    # shell guards (no webview needed) — the CI job runs this
make desktop-test-live ORCHICON_DESKTOP_PLANE_URL=http://127.0.0.1:8080   # transport proof
```

Then `./bin/orchicon-desktop --plane http://127.0.0.1:8091` (or set `ORCHICON_DESKTOP_PLANE_URL`);
with neither, it points at the dev instance's control port.

**CI**: a separate `desktop-shell` job (not `ci-go`, which is `CGO_ENABLED=0`-shaped). It compiles the
entry point with the release's own tags (`desktop,production,webkit2_41`), so a Linux cgo break is a CI
failure rather than a release-time surprise — without `production` Wails compiles a stub and no cgo file is
built at all.
**Release**: `.github/workflows/release.yml`'s `desktop` job builds per OS on `ubuntu/macos/windows` runners.
**Linux ABI tag**: `webkit2_41` selects WebKitGTK 4.1 (`libwebkit2gtk-4.1-dev`). Wails chooses the ABI by
build tag, so the tags and the apt package must agree or the Linux build cannot find its pkg-config file.
**`CROSS_PLATFORMS` is untouched** — the app is CGO + a native webview and cannot come from a
`CGO_ENABLED=0` cross-compile (`TestDesktopNotInCrossPlatforms`).
