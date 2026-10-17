// Container engine availability, as the plane reports it on GET /enginez.
//
// WHY A RAW FETCH AND NOT A CONNECT CLIENT: /enginez is a plain JSON operational endpoint
// next to /versionz (internal/api/api.go), not an RPC. Adding a proto message for one status
// blob would mean regenerating every client for information that is not part of the domain
// model — it describes the HOST the plane runs on.
//
// The three states are the vocabulary the installer uses too, so what the operator reads here
// is the decision `orchicon install` actually made:
//   ready        — a working engine answered (its version is shown)
//   provisioning — podman is being installed right now (installer marker file)
//   unavailable  — no engine, too old a podman, or a machine that will not start (reason given)
import { useQuery } from "@tanstack/react-query";

export type ContainerEngineState = "ready" | "provisioning" | "unavailable";

export type ContainerEngineStatus = {
  engine: string;
  bin?: string;
  version?: string;
  minimum: string;
  available: boolean;
  state: ContainerEngineState;
  reason?: string;
  machine?: string;
};

export async function fetchEngineStatus(): Promise<ContainerEngineStatus> {
  const res = await fetch("/enginez", { headers: { accept: "application/json" } });
  if (!res.ok) throw new Error(`/enginez: ${res.status}`);
  return (await res.json()) as ContainerEngineStatus;
}

/**
 * Polls the plane's own availability report. `retry: false` because a plane that cannot
 * answer this endpoint is not going to answer it three times; the badge simply says it does
 * not know rather than staying blank.
 */
export function useContainerEngineStatus() {
  return useQuery({
    queryKey: ["container-engine-status"],
    queryFn: fetchEngineStatus,
    staleTime: 15_000,
    refetchInterval: 30_000,
    retry: false,
  });
}

/** The one-line sentence rendered under the execution-mode select. */
export function describeEngineStatus(status?: ContainerEngineStatus): string {
  if (!status) return "Checking whether containers are available…";
  if (status.state === "provisioning") return "Provisioning containers — podman is being installed…";
  if (status.available) {
    const version = status.version ? ` ${status.version}` : "";
    const machine =
      status.machine && status.machine !== "not-applicable" && status.machine !== "running"
        ? `, machine ${status.machine}`
        : "";
    return `Containers ready (${status.engine}${version}${machine})`;
  }
  const reason = status.reason || "no container engine";
  return `Containers unavailable — ${reason}. Runs will execute locally.`;
}
