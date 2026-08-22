import { useEffect, useState } from "react";
import type { Timestamp } from "@bufbuild/protobuf";
import { formatElapsed } from "@/lib/format";

function toMs(val: Timestamp | number | null | undefined): number {
  if (val == null) return 0;
  if (typeof val === "number") return val;
  return Number(val.seconds) * 1000 + (val.nanos ?? 0) / 1_000_000;
}

export function LiveDuration({
  startedAt,
  endedAt,
  now,
  className,
}: {
  startedAt?: Timestamp | number | null | null;
  endedAt?: Timestamp | number | null;
  now?: number;
  className?: string;
}) {
  const [internalNow, setInternalNow] = useState(0);

  const startMs = toMs(startedAt);
  const endMs = toMs(endedAt);

  useEffect(() => {
    if (now !== undefined) return;
    if (endMs) return;
    const tick = () => setInternalNow(Date.now());
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [now, endMs]);

  if (!startMs) return null;

  const effectiveNow = now !== undefined ? now : internalNow;
  const elapsed = endMs ? (endMs - startMs) / 1000 : (effectiveNow - startMs) / 1000;

  return (
    <span className={className ?? "font-mono text-xs text-muted-foreground shrink-0"}>
      {formatElapsed(Math.max(0, elapsed))}
    </span>
  );
}
