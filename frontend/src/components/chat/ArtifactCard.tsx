import { useState } from "react";
import { Markdown } from "@/components/markdown";

interface ArtifactCardProps {
  artifact: { name: string; type: string; content: string };
  className?: string;
}

export function ArtifactCard({ artifact, className }: ArtifactCardProps) {
  const fileName = artifact.name.split("/").pop() || artifact.name;
  const isMarkdown =
    artifact.type === "markdown" || fileName.endsWith(".md");
  const [open, setOpen] = useState(false);

  return (
    <div className={`flex justify-start pl-2 ${className ?? ""}`}>
      <div className="w-full max-w-[92%] overflow-hidden rounded-xl border border-sky-300/40 bg-sky-50/30 dark:bg-sky-950/20">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="flex w-full items-center gap-2 px-3 py-2 text-left"
        >
          <span className="rounded bg-sky-200 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wide dark:bg-sky-900">
            artifact
          </span>
          <span className="truncate font-mono text-xs">{fileName}</span>
          <span className="ml-auto shrink-0 text-[10px] text-muted-foreground">
            {artifact.content.length.toLocaleString()} bytes
          </span>
        </button>
        {open && (
          <div className="border-t border-border/50 p-3">
            {isMarkdown ? (
              <div className="max-h-48 overflow-auto rounded bg-background/70 p-2 text-xs leading-relaxed">
                <Markdown>{artifact.content}</Markdown>
              </div>
            ) : (
              <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded bg-background/70 p-2 font-mono text-[11px] leading-relaxed">
                {artifact.content}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
