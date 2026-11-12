import { useContainerEngineStatus, describeEngineStatus } from "@/api/engine";

/**
 * The availability badge shown where execution mode is chosen (AC5).
 *
 * IT IS NEVER A TOAST AND NEVER A SILENT DOWNGRADE. Choosing "runtime" while containers are
 * unavailable would otherwise look like it worked, and the operator would find out when the
 * first run executed in-process instead. The reason is rendered inline, next to the choice
 * that depends on it.
 */
export function EngineAvailability() {
  const { data, isError } = useContainerEngineStatus();

  const text = isError
    ? "Container availability unknown — the plane did not answer /enginez."
    : describeEngineStatus(data);
  const tone = isError
    ? "text-muted-foreground"
    : data?.available
      ? "text-muted-foreground"
      : data?.state === "provisioning"
        ? "text-muted-foreground"
        : "text-amber-500";

  return (
    <p className={`text-xs ${tone}`} data-testid="engine-availability" role="status">
      {text}
    </p>
  );
}
