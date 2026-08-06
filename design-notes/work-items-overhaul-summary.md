# Work Items Page Overhaul — Design Summary

## Overview

Complete UI and functionality overhaul of the Work Items page to achieve Jira-like quality. This document summarizes all design decisions for the implementation worker.

## Files to Modify

### Critical Bug Fixes
1. **`frontend/src/components/work-items/work-items-board.tsx`** — Main board component
   - Add `MANUALLY_UNMOVABLE_STATUSES` gate in `handleMove()`
   - Fix drop target resolution in `handleDragEnd()` (use `columnForStatus()` for card drops)
   - Add optimistic cache updates
   - Fix board sizing (fill viewport)

2. **`frontend/src/components/work-items/work-item-meta.ts`** — Status metadata
   - Add `MANUALLY_UNMOVABLE_STATUSES` constant
   - Export for use in board and filter bar

3. **`frontend/src/routes/work-items.tsx`** — Page shell
   - Pass `queryClient` to board for optimistic updates
   - Add transition animation state

### Visual Improvements
4. **`frontend/src/components/work-items/work-items-tree.tsx`** — Tree view
   - Add sticky column headers
   - Improve indent guides (solid lines + junction dots)
   - Add row hover with left border accent

5. **`frontend/src/components/work-items/work-items-filter-bar.tsx`** — Filter bar
   - Two-row layout (Row 1: search + filters, Row 2: sort + view + actions)
   - Add quick filter chips (clickable status dots)

6. **`frontend/src/components/work-items/work-item-card.tsx`** — Card design
   - Enhance hover state
   - Improve moving state animation

## Design Decisions

### 1. Running Status Restriction
- RUNNING, CHECKPOINTING, RECOVERING are never drag targets
- Running column shows "System-managed" label
- No drop highlight on Running column
- `MoveToMenu` select excludes these statuses

### 2. Board Layout
- Board fills viewport: `min-h-[calc(100vh-280px)]`
- Column minimum: `min-w-[240px]` (was 200px)
- Column body: `flex-1 overflow-y-auto`
- Empty columns show dashed border drop zone

### 3. Drop Target Resolution
- When dropping on a card, resolve via `columnForStatus(overData.status)`
- This ensures SCHEDULED items in Pending column map to PENDING, not SCHEDULED

### 4. Optimistic Updates
- On drag-end, immediately move card to target column in cache
- Server confirms with real data
- On error, invalidate query to revert

### 5. Tree View
- Solid indent guides with junction dots
- Sticky column headers (Title, Status)
- Row hover with left border accent
- Enhanced checkbox feedback (colored background)

### 6. Filter Bar
- Two-row layout for better organization
- Quick filter chips (click status dot in column header)
- Clear all filters button

## Accessibility

- All drag operations have keyboard equivalents (Move to... select)
- ARIA labels on columns and cards
- Focus management after drag operations
- Color contrast meets WCAG AA (4.5:1)
- Reduced motion support via `motion-reduce:` classes

## Testing Checklist

- [ ] Cannot drag items to Running column
- [ ] Cannot use "Move to..." to select Running
- [ ] Board fills viewport width
- [ ] Columns are at least 240px wide
- [ ] Drop-to-Pending works correctly (not SCHEDULED)
- [ ] Items don't disappear during workflow moves
- [ ] Optimistic updates show card in new column immediately
- [ ] Tree view has sticky headers
- [ ] Tree view indent guides are visible
- [ ] Filter bar two-row layout works on all screen sizes
- [ ] Quick filter chips toggle status filter
- [ ] All keyboard interactions work
- [ ] Screen reader announcements work
