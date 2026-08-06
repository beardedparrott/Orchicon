# ADR-10: Tree View and Filter Bar Improvements

## Status

Proposed

## Context

The Tree view and Filter bar need improvements to match Jira-like quality:

1. **Tree indent guides**: Current dashed lines are hard to follow across long rows
2. **No sticky headers**: Column context (Kind, Status) is lost when scrolling
3. **Filter bar wrapping**: All controls in one row wrap unpredictably on medium screens
4. **No quick filters**: Users can't click a status dot in a column header to filter
5. **Cascade checkbox feedback**: The tri-state logic is correct but visual feedback is subtle

## Decision

### 1. Tree View Enhancements

**Solid indent guides with junction dots:**

Replace the dashed border-left guides with solid lines and kind-colored dots at hierarchy junctions:

```tsx
{/* Indent guide with junction dot */}
{Array.from({ length: depth }, (_, i) => (
  <span key={i} aria-hidden className="relative h-full w-[18px] shrink-0">
    {/* Vertical line */}
    <span className="absolute left-[8px] top-0 h-full w-px bg-border/60" />
    {/* Junction dot at the bottom of this level */}
    {i === depth - 1 && (
      <span className="absolute bottom-[10px] left-[5px] h-[7px] w-[7px] rounded-full bg-border/60" />
    )}
  </span>
))}
```

**Sticky column headers (table-like):**

Wrap the tree in a virtual table structure with sticky headers:

```tsx
<div className="overflow-x-auto">
  <div className="min-w-[640px]">
    {/* Sticky header row */}
    <div className="sticky top-0 z-10 flex items-center gap-1.5 border-b bg-card/90 px-1.5 py-1.5 text-xs font-medium text-muted-foreground backdrop-blur">
      <span className="w-5" /> {/* checkbox space */}
      <span className="w-5" /> {/* expand space */}
      <span className="w-5" /> {/* kind badge space */}
      <span className="flex-1">Title</span>
      <span className="w-20 text-right">Status</span>
    </div>
    {/* Tree rows */}
    {roots.map((item) => (
      <TreeNode key={item.id} ... />
    ))}
  </div>
</div>
```

**Row hover with left border:**

```tsx
<div className={cn(
  "flex items-center gap-1.5 rounded-md border border-transparent px-1.5 py-1.5 transition-colors",
  "hover:border-l-2 hover:border-l-primary hover:bg-accent/50",
  selected.has(item.id) && "bg-accent/60",
)}>
```

### 2. Filter Bar Two-Row Layout

**Before:** Single flex-wrap row

**After:** Two-row layout with clear grouping

```tsx
<div className="space-y-3">
  {/* Row 1: Search + primary filters */}
  <div className="flex flex-wrap items-center gap-3">
    <select ...> {/* Project */} </select>
    <div className="relative min-w-[160px] flex-1">
      <Search ... />
      <Input ... />
    </div>
    <select ...> {/* Status */} </select>
    <select ...> {/* Kind */} </select>
  </div>
  
  {/* Row 2: Sort + view + actions */}
  <div className="flex flex-wrap items-center gap-3">
    <select ...> {/* Sort by */} </select>
    <select ...> {/* Sort order */} </select>
    
    <div className="flex rounded-md border" role="group">
      <button ...>Tree</button>
      <button ...>Board</button>
    </div>
    
    {projectId && (
      <Button variant="outline" size="sm" asChild>
        <Link to="/work-items/graph">Dependency Graph</Link>
      </Button>
    )}
    
    <div className="ml-auto flex items-center gap-2">
      <input type="checkbox" ... />
      <span ...>{count}</span>
      {selectedCount > 0 && <Button ...>Delete</Button>}
    </div>
  </div>
</div>
```

**Quick filter chips:**

Add clickable status dots in the board column headers that toggle the status filter:

```tsx
{/* In BoardColumn header */}
<button
  onClick={() => onStatusFilter(column.status)}
  className={cn(
    "h-2 w-2 rounded-full transition-colors",
    statusMeta(column.status).dot,
    activeFilter === column.status && "ring-2 ring-ring ring-offset-1",
  )}
  aria-label={`Filter by ${column.label}`}
/>
```

### 3. Enhanced Cascade Checkbox Feedback

**Current:** Checkbox changes between checked/indeterminate/unchecked

**Enhanced:** Add subtle background color to indicate selection cascade:

```tsx
<div className={cn(
  "rounded-md px-1.5 py-1.5 transition-colors",
  checked && "bg-primary/5",
  triState && "bg-primary/3",
)}>
  <input
    type="checkbox"
    checked={checked}
    ref={(el) => { if (el) el.indeterminate = triState; }}
    className={cn(
      "h-4 w-4 rounded border-input",
      checked && "border-primary bg-primary text-primary-foreground",
      triState && "border-primary bg-primary/20",
    )}
  />
</div>
```

## Consequences

### Positive
- Tree view is easier to scan with solid guides and sticky headers
- Filter bar is more organized with two-row layout
- Quick filter chips provide faster filtering workflow
- Cascade checkbox feedback is more visible

### Negative
- Two-row filter bar takes more vertical space (mitigated by sticky positioning)
- Solid indent guides are slightly more complex than dashed borders
- Quick filter chips add interactive elements to column headers

### Risks
- Sticky headers may conflict with the board's horizontal scroll (mitigated by `z-index`)
- Two-row layout may not fit on very small screens (mitigated by `flex-wrap`)
