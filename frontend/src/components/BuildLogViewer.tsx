import { useEffect, useRef } from "react";
import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";

export function BuildLogViewer({ log, title = "Build log" }: { log: string; title?: string }) {
  const ref = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [log]);
  const copy = async () => {
    try { await navigator.clipboard.writeText(log); } catch {}
  };
  if (!log) return null;
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <p className="text-sm font-medium">{title}</p>
        <Button variant="outline" size="sm" onClick={copy} aria-label="Copy build log">
          <Copy className="mr-1 h-3 w-3" /> Copy
        </Button>
      </div>
      <pre
        ref={ref}
        className="max-h-[32rem] overflow-y-auto whitespace-pre-wrap break-words rounded-md border bg-zinc-950 p-3 font-mono text-xs text-zinc-100"
        aria-label="Build log"
      >
        {log}
      </pre>
    </div>
  );
}
