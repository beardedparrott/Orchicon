import { useEffect, useMemo, useState } from "react";
import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

import { Button } from "@/components/ui/button";

interface EntityYamlViewProps {
  data: Record<string, unknown>;
  title?: string;
  onClone?: () => void;
  cloneDisabled?: boolean;
  cloneLabel?: string;
  showClone?: boolean;
  editable?: boolean;
  onSave?: (parsed: Record<string, unknown>) => void;
  saveLabel?: string;
  saveDisabled?: boolean;
}

export function EntityYamlView({
  data,
  title,
  onClone,
  cloneDisabled,
  cloneLabel = "Clone",
  showClone = true,
  editable = false,
  onSave,
  saveLabel = "Save YAML",
  saveDisabled = false,
}: EntityYamlViewProps) {
  const generated = useMemo(() => {
    try {
      return stringifyYaml(data, { lineWidth: 0, indent: 2, sortMapEntries: false });
    } catch {
      return "# failed to serialize";
    }
  }, [data]);
  const [code, setCode] = useState(generated);
  const [parseErr, setParseErr] = useState("");

  useEffect(() => { setCode(generated); setParseErr(""); }, [generated]);

  const handleSave = () => {
    setParseErr("");
    try {
      const parsed = parseYaml(code);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        onSave?.(parsed as Record<string, unknown>);
      } else {
        setParseErr("YAML must be a mapping (key-value pairs)");
      }
    } catch (err) {
      setParseErr(String(err));
    }
  };

  return (
    <div className="h-[480px] rounded-lg border bg-card">
      <div className="flex items-center justify-between border-b px-4 py-2 text-xs text-muted-foreground">
        <span>{title || "YAML"}</span>
        <div className="flex items-center gap-2">
          {showClone && onClone && (
            <Button variant="outline" size="sm" onClick={onClone} disabled={cloneDisabled} className="h-7 text-xs">
              {cloneDisabled ? "Cloning…" : cloneLabel}
            </Button>
          )}
          {editable && onSave && (
            <Button variant="default" size="sm" onClick={handleSave} disabled={saveDisabled} className="h-7 text-xs">
              {saveDisabled ? "Saving…" : saveLabel}
            </Button>
          )}
        </div>
      </div>
      {parseErr && (
        <div className="border-b border-rose-300 bg-rose-50 px-4 py-1.5 text-xs text-rose-700 dark:border-rose-800 dark:bg-rose-950/40 dark:text-rose-300">
          {parseErr}
        </div>
      )}
      <textarea
        className="h-[calc(100%-36px)] w-full resize-none border-0 bg-transparent p-4 font-mono text-xs leading-relaxed outline-none"
        value={code}
        onChange={(e) => { setCode(e.target.value); setParseErr(""); }}
        readOnly={!editable}
        spellCheck={false}
      />
    </div>
  );
}
