// RuntimeImageSelect — the work-item runtime_image dropdown (shared by
// the new + edit work item forms). Options come from the daemon's stock
// images plus the tenant's ready custom images; the base image is the
// default.

import { Link } from "@tanstack/react-router";

import { useAvailableRuntimeImages, useListRuntimeImages } from "@/api/runtimeImages";
import { RuntimeImageStatus } from "@/api/gen/orchicon/api/v1/runtime_image_pb";

/**
 * RuntimeImageSelect — the work item's (or recurring item's) runtime_image picker.
 *
 * MODE-AWARE, and that is the point of the `mode` prop.
 *
 * A LOCAL run has no container, so it has no image. Before this, the control lied about
 * that in two ways, both reported by the operator:
 *
 *   1. It COERCED an empty value onto the default image (`value={value || defaultImage}`),
 *      so an item with NO image displayed a concrete one — "I set it to local and it set
 *      it to 'default (base image)'".
 *   2. Worse, that coercion was not cosmetic: the form then SUBMITTED the coerced value,
 *      so every GUI save of a local item round-tripped an image back into a field the
 *      server had just cleared. That is how the server-side clear kept getting undone.
 *
 * So: when the item is LOCAL the picker is DISABLED and states that a local run uses no
 * image, and `onChange("")` is offered as a real "No image" option for the RUNTIME case
 * (which is a legitimate choice — "inherit/use the base image").
 *
 * `mode` is optional: omitted means "unknown", and the control behaves as a plain picker
 * (no mode-dependent disabling) so a caller that has not been updated cannot break.
 */
export function RuntimeImageSelect({
  value,
  onChange,
  mode,
}: {
  value: string;
  onChange: (v: string) => void;
  /** The item's effective execution mode. "local" disables the picker: no container, no image. */
  mode?: "local" | "runtime";
}) {
  const { data } = useAvailableRuntimeImages();
  // The available-image list carries only REFS; the rows carry STATUS. They are
  // cross-referenced by tag so the picker can say when the chosen image is not
  // downloaded on this host (status ABSENT). Runtime images are fetched at
  // opt-in, never mid-run, so this — the place an item opts into containers —
  // is where the fetch has to be surfaced; otherwise the first run fails at
  // start with the ref and the fix, which is later than the operator wanted to
  // learn it.
  const { data: rows } = useListRuntimeImages();
  const stock = data?.stockImages ?? [];
  const custom = data?.customImages ?? [];
  const options = [...stock, ...custom].filter(
    (img, i, arr) => img && arr.indexOf(img) === i,
  );
  const defaultImage = data?.defaultImage || stock[0] || "";
  // NO COERCION. `chosen` is what the operator actually has, so the "not downloaded"
  // warning describes the REAL value rather than a fallback it was silently given.
  const chosen = value;
  const chosenRow = (rows ?? []).find((r) => r?.tag && r.tag === chosen);
  const chosenAbsent = chosenRow?.status === RuntimeImageStatus.ABSENT;
  const isLocal = mode === "local";

  if (isLocal) {
    // A LOCAL RUN HAS NO CONTAINER, so this control has nothing to choose. Saying so — and
    // NOT offering a selectable value — is what stops the "default (base image)" reading
    // and stops the form re-submitting an image the server cleared.
    return (
      <>
        <div className="w-full rounded-xl glass-input px-3 py-1.5 text-sm opacity-70">
          No image — a local run executes on the host, not in a container
        </div>
        <p className="mt-1 text-xs text-muted-foreground">
          Switch the execution mode to <span className="font-medium">runtime</span> to choose
          an image.
        </p>
      </>
    );
  }

  return (
    <>
      <select
        // NO `value || defaultImage`: an empty value is a REAL state (no image chosen;
        // the run uses the base image), and coercing it here is what made the form
        // submit an image nobody picked.
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-xl glass-input px-3 py-1.5 text-sm"
      >
        <option value="">Default ({defaultImage || "base image"})</option>
        {options
          .filter((img) => img && img !== defaultImage)
          .map((img) => (
            <option key={img} value={img}>
              {img}
            </option>
          ))}
      </select>
      {chosenAbsent && chosenRow && (
        <p role="status" className="mt-1 text-xs text-amber-700 dark:text-amber-300">
          <strong>Not downloaded</strong> on this host — a run needing it fails at start.
          {" "}
          <Link
            to="/runtime-images/$id"
            params={{ id: chosenRow.id }}
            className="underline"
          >
            Fetch it
          </Link>
          {" "}
          (a published image is downloaded; a locally-built one is built from its shipped
          template — either way the row ends ready).
        </p>
      )}
      {/* ALWAYS PRESENT, not only when the CHOSEN image is missing.
          The picker can only offer images the daemon already knows about, so an image nobody has
          fetched yet is invisible here — which is a dead end: you cannot select it, so you never
          learn it is missing, so you never fetch it. This link is the way out of that loop, and it
          has to be visible before the choice is made rather than after it has gone wrong. */}
      <p className="mt-1 text-xs text-muted-foreground">
        <Link to="/runtime-images" className="underline">
          Fetch new images
        </Link>
        {" "}— every image we host is listed there, downloaded or not.
      </p>
    </>
  );
}
