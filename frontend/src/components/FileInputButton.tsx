import { useRef } from "react";
import { FileUp } from "lucide-react";

import { Button } from "@/components/ui/button";

interface FileInputButtonProps {
  onLoad: (content: string) => void;
  accept?: string;
  multiple?: boolean;
  label?: string;
}

export function FileInputButton({
  onLoad,
  accept = ".md,.txt,.mdx",
  multiple = false,
  label = "Load from file",
}: FileInputButtonProps) {
  const inputRef = useRef<HTMLInputElement>(null);

  const handleFiles = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (!files || files.length === 0) return;
    const parts: string[] = [];
    for (let i = 0; i < files.length; i++) {
      const text = await files[i].text();
      const name = files[i].name;
      if (multiple && files.length > 1) {
        parts.push(`## ${name}\n\n${text}`);
      } else {
        parts.push(text);
      }
    }
    onLoad(parts.join("\n\n"));
    e.target.value = "";
  };

  return (
    <>
      <input ref={inputRef} type="file" accept={accept} multiple={multiple} onChange={handleFiles} className="hidden" />
      <Button type="button" variant="outline" size="sm" className="text-xs h-7" onClick={() => inputRef.current?.click()}>
        <FileUp className="h-3 w-3 mr-1" />
        {label}
      </Button>
    </>
  );
}
