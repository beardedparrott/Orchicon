import { desktopInfo } from "@/lib/desktop";

/**
 * VersionMismatchBanner — AC6: a mismatch between the SPA this binary shipped and
 * the plane it is talking to is SURFACED, not silently rendered.
 *
 * The desktop app embeds its own UI, so a window built from one tree can talk to a
 * plane built from another; the UI may then be speaking an API the plane does not
 * have. Rendering that quietly is how "the button does nothing" reports are born.
 *
 * It renders NOTHING in a browser and nothing when the versions agree, so it
 * cannot add chrome to the web app.
 */
export function VersionMismatchBanner() {
  const info = desktopInfo();
  if (!info || !info.versionMismatch) return null;
  return (
    <div
      role="alert"
      className="border-b border-destructive/40 bg-destructive/10 px-4 py-2 text-xs text-destructive"
    >
      <strong className="font-semibold">Version mismatch:</strong> this window's UI is{" "}
      <code>{info.appTag}</code> but the plane at <code>{info.planeURL}</code> is{" "}
      <code>{info.planeTag}</code>. This window's UI may not match the plane's API.
    </div>
  );
}
