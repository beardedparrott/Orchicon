import { useState, useRef, useEffect } from "react";
import {
  ChevronRight,
  FolderClosed,
  FolderOpen,
  FolderPlus,
  Pencil,
  Trash2,
} from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import type { Category } from "@/lib/category-store";

interface CategoryFolderProps {
  category: Category;
  count: number;
  isCollapsed: boolean;
  onToggle: () => void;
  onRename: (newName: string) => void;
  onDelete: () => void;
  onUpdateDescription: (description: string) => void;
  children: React.ReactNode;
}

export function CategoryFolder({
  category,
  count,
  isCollapsed,
  onToggle,
  onRename,
  onDelete,
  onUpdateDescription,
  children,
}: CategoryFolderProps) {
  const [isEditing, setIsEditing] = useState(false);
  const [editName, setEditName] = useState(category.name);
  const [isEditingDesc, setIsEditingDesc] = useState(false);
  const [editDesc, setEditDesc] = useState(category.description ?? "");
  const [showActions, setShowActions] = useState(false);
  const nameInputRef = useRef<HTMLInputElement>(null);
  const descInputRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    if (isEditing && nameInputRef.current) {
      nameInputRef.current.focus();
      nameInputRef.current.select();
    }
  }, [isEditing]);

  useEffect(() => {
    if (isEditingDesc && descInputRef.current) {
      descInputRef.current.focus();
    }
  }, [isEditingDesc]);

  const handleRenameSubmit = () => {
    const trimmed = editName.trim();
    if (trimmed && trimmed !== category.name) {
      onRename(trimmed);
    } else {
      setEditName(category.name);
    }
    setIsEditing(false);
  };

  const handleDescSubmit = () => {
    onUpdateDescription(editDesc.trim());
    setIsEditingDesc(false);
  };

  const isUncategorized = category.id === "uncategorized";

  return (
    <div className="mt-3 first:mt-0">
      <div
        className={cn(
          "group flex items-center gap-2 rounded-lg px-3 py-2 transition-colors",
          "bg-muted/50 hover:bg-muted/80",
        )}
        onMouseEnter={() => setShowActions(true)}
        onMouseLeave={() => setShowActions(false)}
      >
        <button
          type="button"
          onClick={onToggle}
          className="flex items-center gap-2 shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 rounded"
          aria-expanded={!isCollapsed}
          aria-controls={`folder-content-${category.id}`}
        >
          <ChevronRight
            className={cn(
              "h-4 w-4 text-muted-foreground transition-transform duration-200",
              !isCollapsed && "rotate-90",
            )}
          />
          {isCollapsed ? (
            <FolderClosed className="h-4 w-4 text-muted-foreground" />
          ) : (
            <FolderOpen className="h-4 w-4 text-muted-foreground" />
          )}
        </button>

        {isEditing ? (
          <Input
            ref={nameInputRef}
            value={editName}
            onChange={(e) => setEditName(e.target.value)}
            onBlur={handleRenameSubmit}
            onKeyDown={(e) => {
              if (e.key === "Enter") handleRenameSubmit();
              if (e.key === "Escape") {
                setEditName(category.name);
                setIsEditing(false);
              }
            }}
            className="h-6 text-sm font-semibold"
            maxLength={64}
          />
        ) : (
          <button
            type="button"
            onClick={onToggle}
            className="flex items-center gap-2 min-w-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 rounded"
          >
            <span className="text-sm font-semibold truncate">
              {category.name}
            </span>
            <span className="rounded-full bg-secondary text-secondary-foreground px-2 py-0.5 text-xs">
              {count}
            </span>
          </button>
        )}

        <div className="ml-auto flex items-center gap-1">
          {!isUncategorized && showActions && !isEditing && (
            <>
              <button
                type="button"
                onClick={() => setIsEditing(true)}
                className="rounded p-1 text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
                title="Rename folder"
              >
                <Pencil className="h-3.5 w-3.5" />
              </button>
              <button
                type="button"
                onClick={onDelete}
                className="rounded p-1 text-muted-foreground hover:text-destructive hover:bg-accent transition-colors"
                title="Delete folder"
              >
                <Trash2 className="h-3.5 w-3.5" />
              </button>
            </>
          )}
        </div>
      </div>

      {category.description && !isEditing && !isEditingDesc && (
        <div className="ml-9 mt-0.5 flex items-center gap-1">
          <p
            className="text-xs text-muted-foreground italic cursor-pointer hover:text-foreground truncate max-w-md"
            onClick={() => {
              setEditDesc(category.description ?? "");
              setIsEditingDesc(true);
            }}
            title="Click to edit description"
          >
            {category.description}
          </p>
        </div>
      )}

      {isEditingDesc && (
        <div className="ml-9 mt-0.5">
          <textarea
            ref={descInputRef}
            value={editDesc}
            onChange={(e) => setEditDesc(e.target.value)}
            onBlur={handleDescSubmit}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                handleDescSubmit();
              }
              if (e.key === "Escape") {
                setIsEditingDesc(false);
              }
            }}
            className="w-full max-w-md rounded border border-input bg-background px-2 py-1 text-xs resize-none"
            rows={2}
            maxLength={256}
            placeholder="Add a description..."
          />
        </div>
      )}

      {!isUncategorized && !category.description && !isEditingDesc && showActions && (
        <div className="ml-9 mt-0.5">
          <button
            type="button"
            onClick={() => {
              setEditDesc("");
              setIsEditingDesc(true);
            }}
            className="text-xs text-muted-foreground hover:text-foreground flex items-center gap-1"
          >
            <FolderPlus className="h-3 w-3" />
            Add description
          </button>
        </div>
      )}

      <div
        id={`folder-content-${category.id}`}
        role="region"
        aria-label={`${category.name}, ${count} items, ${isCollapsed ? "collapsed" : "expanded"}`}
        className={cn("ml-4", isCollapsed && "hidden")}
      >
        {children}
      </div>
    </div>
  );
}
