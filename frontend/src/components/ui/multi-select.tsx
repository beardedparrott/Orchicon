// Multi-select checkbox dropdown (design-notes/visual-and-functional-
// tweaks-to-work-items-page.md, ADR-WI-2).
//
// A reusable list-page filter primitive: a dropdown of checkboxes built
// on @radix-ui/react-dropdown-menu's CheckboxItem. Correct keyboard +
// focus management comes from Radix (arrows move, Enter/Space toggles,
// Escape closes with focus returned to the trigger, typeahead, click-
// outside). Selection composes with OR within the group.
//
// What an EMPTY selection means is the caller's decision (the Work Items
// page treats empty as "show nothing" — the defaults are every option
// selected). Use the `emptyLabel` to describe the empty state accurately.
//
// Kept in components/ui so the other list pages (Approvals, Executions,
// Workers, Policies) can adopt it later.

import * as DropdownMenuPrimitive from "@radix-ui/react-dropdown-menu";
import { Check, ChevronDown } from "lucide-react";

import { cn } from "@/lib/utils";

export interface MultiSelectOption<T extends string | number> {
  value: T;
  label: string;
}

export interface MultiSelectProps<T extends string | number> {
  /** aria-label for the trigger (e.g. "Filter by status") */
  label: string;
  options: MultiSelectOption<T>[];
  selected: Set<T>;
  onChange: (next: Set<T>) => void;
  /** Text shown when nothing is selected (e.g. "All statuses") */
  emptyLabel?: string;
  className?: string;
}

export function MultiSelect<T extends string | number>({
  label,
  options,
  selected,
  onChange,
  emptyLabel = "All",
  className,
}: MultiSelectProps<T>) {
  // Compact trigger summary: "All statuses" when empty, the labels joined
  // when ≤ 2, otherwise "N selected".
  const summary =
    selected.size === 0
      ? emptyLabel
      : selected.size <= 2
        ? options
            .filter((o) => selected.has(o.value))
            .map((o) => o.label)
            .join(", ")
        : `${selected.size} selected`;

  const toggle = (value: T) => {
    const next = new Set(selected);
    if (next.has(value)) next.delete(value);
    else next.add(value);
    onChange(next);
  };

  return (
    <DropdownMenuPrimitive.Root>
      <DropdownMenuPrimitive.Trigger asChild>
        <button
          type="button"
          aria-label={label}
          className={cn(
            "inline-flex h-9 max-w-[16rem] items-center gap-1.5 rounded-md glass-input px-3 text-sm shadow-sm focus-visible:ring-2 focus-visible:ring-ring",
            className,
          )}
        >
          <span className="truncate text-foreground">{summary}</span>
          <ChevronDown
            aria-hidden="true"
            className="ml-auto h-3.5 w-3.5 shrink-0 text-muted-foreground"
          />
        </button>
      </DropdownMenuPrimitive.Trigger>
      <DropdownMenuPrimitive.Portal>
        <DropdownMenuPrimitive.Content
          align="start"
          side="bottom"
          sideOffset={4}
          className="z-50 max-h-[320px] w-56 overflow-y-auto rounded-xl glass-menu p-1 text-popover-foreground"
        >
          <DropdownMenuPrimitive.Label className="px-2 py-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            {label}
          </DropdownMenuPrimitive.Label>
          {options.map((option) => (
            <DropdownMenuPrimitive.CheckboxItem
              key={String(option.value)}
              checked={selected.has(option.value)}
              onSelect={(event) => {
                // Keep the menu open for multi-select (default closes on select).
                event.preventDefault();
                toggle(option.value);
              }}
              className={cn(
                "flex cursor-pointer select-none items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none",
                "focus:bg-accent focus:text-accent-foreground",
                selected.has(option.value) && "text-accent-foreground",
              )}
            >
              <span
                className={cn(
                  "flex h-4 w-4 shrink-0 items-center justify-center rounded-sm border border-input",
                  selected.has(option.value) && "border-primary bg-primary text-primary-foreground",
                )}
              >
                {selected.has(option.value) && <Check aria-hidden="true" className="h-3 w-3" />}
              </span>
              <span className="truncate">{option.label}</span>
            </DropdownMenuPrimitive.CheckboxItem>
          ))}
          <DropdownMenuPrimitive.Separator className="my-1 h-px bg-border" />
          {/* Footer actions as Radix menu items (not plain buttons): the
              menu's roving-tabindex arrow-key navigation only reaches
              registered items, so plain <button>s in the content were
              unreachable by keyboard. preventDefault keeps the menu open
              for further multi-select changes, matching the checkbox
              items above. */}
          <div className="flex items-center justify-between px-1 py-0.5">
            <DropdownMenuPrimitive.Item
              onSelect={(event) => {
                event.preventDefault();
                onChange(new Set(options.map((o) => o.value)));
              }}
              className="flex cursor-pointer select-none items-center rounded-sm px-2 py-1.5 text-xs font-medium text-muted-foreground outline-none focus:bg-accent focus:text-accent-foreground data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground"
            >
              Select all
            </DropdownMenuPrimitive.Item>
            <DropdownMenuPrimitive.Item
              onSelect={(event) => {
                event.preventDefault();
                onChange(new Set());
              }}
              className="flex cursor-pointer select-none items-center rounded-sm px-2 py-1.5 text-xs font-medium text-muted-foreground outline-none focus:bg-accent focus:text-accent-foreground data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground"
            >
              Clear
            </DropdownMenuPrimitive.Item>
          </div>
        </DropdownMenuPrimitive.Content>
      </DropdownMenuPrimitive.Portal>
    </DropdownMenuPrimitive.Root>
  );
}
