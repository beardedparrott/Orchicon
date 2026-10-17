// RuntimeImageSelect — the work-item runtime_image dropdown (shared by
// the new + edit work item forms). Options come from the daemon's stock
// images plus the tenant's ready custom images; the base image is the
// default.

import { Link } from "@tanstack/react-router";

import { useAvailableRuntimeImages, useListRuntimeImages } from "@/api/runtimeImages";
import { RuntimeImageStatus } from "@/api/gen/orchicon/api/v1/runtime_image_pb";

export function RuntimeImageSelect({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
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
  const chosen = value || defaultImage;
  const chosenRow = (rows ?? []).find((r) => r?.tag && r.tag === chosen);
  const chosenAbsent = chosenRow?.status === RuntimeImageStatus.RUNTIME_IMAGE_STATUS_ABSENT;

  return (
    <>
      <select
        value={value || defaultImage}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-xl glass-input px-3 py-1.5 text-sm"
      >
        <option value={defaultImage}>
          Default ({defaultImage || "base image"})
        </option>
        {options
          .filter((img) => img !== defaultImage)
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
          (downloads the published image; Deploy instead builds it, which is much slower).
        </p>
      )}
    </>
  );
}
