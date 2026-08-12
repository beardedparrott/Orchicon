import type { Category } from "@/lib/category-store";

interface CategoryAssignSelectProps {
  entityId: string;
  currentCategoryId: string | undefined;
  categories: Category[];
  onAssign: (entityId: string, categoryId: string) => void;
}

export function CategoryAssignSelect({
  entityId,
  currentCategoryId,
  categories,
  onAssign,
}: CategoryAssignSelectProps) {
  const sorted = [...categories].sort((a, b) => a.order - b.order);
  const sortedWithUncategorized = [
    ...sorted,
    { id: "uncategorized", name: "Uncategorized", order: Infinity },
  ];

  return (
    <select
      value={currentCategoryId ?? "uncategorized"}
      onChange={(e) => {
        const val = e.target.value;
        // "uncategorized" means remove the assignment
        onAssign(entityId, val === "uncategorized" ? "" : val);
      }}
      className="h-7 rounded border border-input bg-transparent px-1.5 text-xs shadow-sm opacity-0 group-hover:opacity-100 focus:opacity-100 transition-opacity shrink-0"
      title="Move to category"
      onClick={(e) => e.stopPropagation()}
    >
      {sortedWithUncategorized.map((cat) => (
        <option key={cat.id} value={cat.id}>
          {cat.name}
        </option>
      ))}
    </select>
  );
}
