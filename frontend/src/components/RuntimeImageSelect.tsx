// RuntimeImageSelect — the work-item runtime_image dropdown (shared by
// the new + edit work item forms). Options come from the daemon's stock
// images plus the tenant's ready custom images; the base image is the
// default.

import { useAvailableRuntimeImages } from "@/api/runtimeImages";

export function RuntimeImageSelect({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
}) {
  const { data } = useAvailableRuntimeImages();
  const stock = data?.stockImages ?? [];
  const custom = data?.customImages ?? [];
  const options = [...stock, ...custom].filter(
    (img, i, arr) => img && arr.indexOf(img) === i,
  );
  const defaultImage = data?.defaultImage || stock[0] || "";

  return (
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
  );
}
