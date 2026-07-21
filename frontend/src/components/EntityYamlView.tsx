import { useMemo } from "react";
import { stringify as stringifyYaml } from "yaml";

import { Button } from "@/components/ui/button";

interface EntityYamlViewProps {
  data: Record<string, unknown>;
  title?: string;
  onClone?: () => void;
  cloneDisabled?: boolean;
  cloneLabel?: string;
  showClone?: boolean;
}

export function EntityYamlView({
  data,
  title,
  onClone,
  cloneDisabled,
  cloneLabel = "Clone",
  showClone = true,
}: EntityYamlViewProps) {
  const yaml = useMemo(() => {
    try {
      return stringifyYaml(data, { lineWidth: 0, indent: 2, sortMapEntries: false });
    } catch {
      return "# failed to serialize";
    }
  }, [data]);

  return (
    <div className="h-[480px] rounded-lg border bg-card">
      <div className="flex items-center justify-between border-b px-4 py-2 text-xs text-muted-foreground">
        <span>{title || "YAML"}</span>
        {showClone && onClone && (
          <Button
            variant="outline"
            size="sm"
            onClick={onClone}
            disabled={cloneDisabled}
            className="h-7 text-xs"
          >
            {cloneDisabled ? "Cloning…" : cloneLabel}
          </Button>
        )}
      </div>
      <textarea
        className="h-[calc(100%-36px)] w-full resize-none border-0 bg-transparent p-4 font-mono text-xs leading-relaxed outline-none"
        value={yaml}
        readOnly
        spellCheck={false}
      />
    </div>
  );
}
