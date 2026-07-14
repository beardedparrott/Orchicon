// TEMPORARY DIAGNOSTIC — user report: every button on every page is
// non-functional. This module attaches a document-level click listener
// + a global error handler + a fixed-position badge in the corner
// that shows the click count, the last error, and the most recent
// click target. It bypasses React entirely so we can tell whether
// the browser sees clicks at all.
//
// What each piece tells us:
//
//   - click count > 0 after a click: browser sees the event, the
//     bug is downstream (handler not attached, React didn't hydrate,
//     a parent swallowed the event with stopPropagation)
//
//   - click count stays at 0: something is intercepting clicks
//     before document — a CSS overlay, a modal, a Z-index bug, or
//     the page is frozen because React threw during render.
//
//   - last error != "": React crashed during initial render. The
//     "Layout was forced" warning the user reported usually points
//     at this. Look at the lastError text — that's the root cause.
//
// This whole file is meant to be removed once the real bug is
// identified. Do not keep it in production.

interface DiagState {
  count: number;
  lastTarget: string;
  lastError: string;
  lastEvent: string;
  preventDefaultCount: number;
  pathname: string;
  routeId: string;
  outletChildCount: number;
  navTransitions: number;
}

const diag: DiagState = {
  count: 0,
  lastTarget: "(no click yet)",
  lastError: "",
  lastEvent: "",
  preventDefaultCount: 0,
  pathname: location.pathname,
  routeId: "(unknown)",
  outletChildCount: 0,
  navTransitions: 0,
};

function describeTarget(t: EventTarget | null): string {
  if (!(t instanceof Element)) return String(t);
  const tag = t.tagName.toLowerCase();
  const id = t.id ? `#${t.id}` : "";
  const cls = t.className && typeof t.className === "string"
    ? "." + t.className.trim().split(/\s+/).slice(0, 3).join(".")
    : "";
  const text = (t.textContent ?? "").trim().slice(0, 40);
  return `${tag}${id}${cls}${text ? ` "${text}"` : ""}`;
}

document.addEventListener(
  "click",
  (e) => {
    diag.count += 1;
    diag.lastTarget = describeTarget(e.target);
    diag.lastEvent = `click @ (${e.clientX},${e.clientY}) defaultPrevented=${e.defaultPrevented}`;
    const origPrevent = e.preventDefault.bind(e);
    let pdCalled = false;
    e.preventDefault = function () {
      pdCalled = true;
      return origPrevent();
    };
    queueMicrotask(() => {
      if (pdCalled) diag.preventDefaultCount += 1;
      render();
    });
    render();
  },
  true
);

window.addEventListener("error", (e) => {
  diag.lastError = `${e.message} (${(e.filename ?? "").split("/").pop()}:${e.lineno})`;
  render();
});

window.addEventListener("unhandledrejection", (e) => {
  const r = e.reason;
  diag.lastError = `unhandledrejection: ${r instanceof Error ? r.message : String(r)}`;
  render();
});

// Watch the URL via popstate / pushState monkeypatch + intervals, so the
// diagnostic badge always shows the current pathname and the rendered
// Outlet's child count (a proxy for "did the new component actually
// mount?"). popstate fires on history.back/forward; pushState/replaceState
// don't fire any event so we patch them.
function readPath() {
  return location.pathname + location.search;
}
const _push = history.pushState.bind(history);
const _replace = history.replaceState.bind(history);
history.pushState = function (...args: Parameters<typeof _push>) {
  const r = _push(...args);
  diag.pathname = readPath();
  diag.navTransitions += 1;
  queueMicrotask(() => {
    diag.pathname = readPath();
    countOutlet();
    render();
  });
  return r;
};
history.replaceState = function (...args: Parameters<typeof _replace>) {
  const r = _replace(...args);
  diag.pathname = readPath();
  queueMicrotask(() => {
    diag.pathname = readPath();
    countOutlet();
    render();
  });
  return r;
};
window.addEventListener("popstate", () => {
  diag.pathname = readPath();
  diag.navTransitions += 1;
  queueMicrotask(() => {
    diag.pathname = readPath();
    countOutlet();
    render();
  });
});

// Count what's actually rendered in the Outlet's <main> tag. If this
// stays 0 after a navigation, the route's component failed to mount.
function countOutlet() {
  const main = document.querySelector("main");
  diag.outletChildCount = main ? main.children.length : 0;
  const headings = main ? Array.from(main.querySelectorAll("h1, h2, h3")).slice(0, 3) : [];
  diag.routeId = headings.map((h) => h.textContent?.trim().slice(0, 32) ?? "").join(" / ") || "(no h1/h2/h3 in main)";
}

const BADGE_ID = "__orchicon_click_diag__";
function ensureBadge(): HTMLDivElement {
  let el = document.getElementById(BADGE_ID) as HTMLDivElement | null;
  if (el) return el;
  el = document.createElement("div");
  el.id = BADGE_ID;
  el.style.cssText = [
    "position:fixed",
    "bottom:12px",
    "left:12px",
    "z-index:2147483647",
    "background:#0b0d12",
    "color:#e6e8ee",
    "border:1px solid #2a3140",
    "border-radius:8px",
    "padding:8px 10px",
    "font:11px/1.4 ui-monospace,monospace",
    "max-width:560px",
    "white-space:pre-wrap",
    "pointer-events:none",
    "opacity:0.95",
  ].join(";");
  el.setAttribute("data-diag", "orchicon-click-counter");
  document.body.appendChild(el);
  return el;
}

function render() {
  const el = ensureBadge();
  el.textContent =
    `orchicon click diag\n` +
    `url:    ${diag.pathname}  (transitions: ${diag.navTransitions})\n` +
    `clicks: ${diag.count}  (preventDefault’d: ${diag.preventDefaultCount})\n` +
    `last:   ${diag.lastEvent}\n` +
    `target: ${diag.lastTarget}\n` +
    `main children: ${diag.outletChildCount}  headings: ${diag.routeId}\n` +
    `error:  ${diag.lastError || "(none)"}`;
}

// Initial sample
queueMicrotask(() => {
  countOutlet();
  render();
});
// And keep sampling in case the Outlet updates without a navigation
setInterval(() => {
  if (location.pathname !== diag.pathname) {
    diag.pathname = location.pathname;
    countOutlet();
    render();
  }
}, 500);

ensureBadge();
render();

// Expose for DevTools poking
(window as unknown as Record<string, unknown>).__orchiconDiag = diag;
