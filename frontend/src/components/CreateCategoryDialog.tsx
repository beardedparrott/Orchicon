import { useState, useRef, useEffect } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

interface CreateCategoryDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreate: (name: string, description?: string) => void;
  existingNames: string[];
}

export function CreateCategoryDialog({
  open,
  onOpenChange,
  onCreate,
  existingNames,
}: CreateCategoryDialogProps) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState("");
  const dialogRef = useRef<HTMLDialogElement>(null);
  const nameInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;

    if (open) {
      dialog.showModal();
      // Focus the name input after the dialog opens
      requestAnimationFrame(() => nameInputRef.current?.focus());
    } else {
      dialog.close();
    }
  }, [open]);

  const handleClose = () => {
    setName("");
    setDescription("");
    setError("");
    onOpenChange(false);
  };

  const validate = (): boolean => {
    const trimmed = name.trim();
    if (!trimmed) {
      setError("Name is required");
      return false;
    }
    if (trimmed.length > 64) {
      setError("Name must be 64 characters or less");
      return false;
    }
    if (trimmed.toLowerCase() === "uncategorized") {
      setError('"Uncategorized" is a reserved name');
      return false;
    }
    if (
      existingNames.some((n) => n.toLowerCase() === trimmed.toLowerCase())
    ) {
      setError("A category with this name already exists");
      return false;
    }
    return true;
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!validate()) return;
    onCreate(name.trim(), description.trim() || undefined);
    handleClose();
  };

  return (
    <dialog
      ref={dialogRef}
      onClose={handleClose}
      className={cn(
        "rounded-lg border bg-background p-0 shadow-lg backdrop:bg-black/50",
        "w-full max-w-md",
      )}
      onClick={(e) => {
        // Close on backdrop click
        if (e.target === dialogRef.current) handleClose();
      }}
    >
      <form onSubmit={handleSubmit} className="p-6">
        <h2 className="text-lg font-semibold mb-4">Create Category</h2>

        <div className="space-y-4">
          <div>
            <label htmlFor="cat-name" className="block text-sm font-medium mb-1.5">
              Name <span className="text-destructive">*</span>
            </label>
            <Input
              ref={nameInputRef}
              id="cat-name"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                if (error) setError("");
              }}
              placeholder="Category name"
              maxLength={64}
              aria-invalid={!!error}
              aria-describedby={error ? "cat-name-error" : undefined}
            />
            {error && (
              <p id="cat-name-error" className="mt-1 text-xs text-destructive">
                {error}
              </p>
            )}
          </div>

          <div>
            <label htmlFor="cat-desc" className="block text-sm font-medium mb-1.5">
              Description <span className="text-muted-foreground">(optional)</span>
            </label>
            <textarea
              id="cat-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="What kind of items go in this category?"
              maxLength={256}
              rows={3}
              className="w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
            />
          </div>
        </div>

        <div className="mt-6 flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={handleClose}>
            Cancel
          </Button>
          <Button type="submit">Create</Button>
        </div>
      </form>
    </dialog>
  );
}
