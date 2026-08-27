import { useEffect, useRef } from "react";
import { Link } from "@tanstack/react-router";
import { X, CheckCheck, Clock, AlertCircle } from "lucide-react";
import { cn } from "@/lib/utils";
import { formatRelativeTime, type NotificationItem } from "./useNotifications";
import { iconForKind, iconForTargetType } from "./notificationIcons";

export function NotificationPanel({
  open,
  items,
  unreadCount,
  isLoading,
  isError,
  onClose,
  onMarkAllRead,
  onClear,
  onItemClick,
  onRetry,
}: {
  open: boolean;
  items: NotificationItem[];
  unreadCount: number;
  isLoading: boolean;
  isError: boolean;
  onClose: () => void;
  onMarkAllRead: () => void;
  onClear: () => void;
  onItemClick: (item: NotificationItem) => void;
  onRetry: () => void;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    requestAnimationFrame(() => {
      const first = containerRef.current?.querySelector<HTMLElement>('button, [role="menuitem"], [href]');
      first?.focus();
    });
  }, [open]);
  if (!open) return null;
  const getFocusable = (): HTMLElement[] => {
    if (!containerRef.current) return [];
    return Array.from(containerRef.current.querySelectorAll<HTMLElement>('button:not([disabled]), [role="menuitem"], a[href]')).filter((el) => el.tabIndex !== -1);
  };
  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const els = Array.from(containerRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
      if (els.length === 0) return;
      const active = document.activeElement as HTMLElement | null;
      let idx = els.indexOf(active as HTMLElement);
      if (idx === -1) idx = e.key === "ArrowDown" ? -1 : 0;
      const next = e.key === "ArrowDown" ? (idx + 1) % els.length : (idx - 1 + els.length) % els.length;
      els[next]?.focus();
    } else if (e.key === "Home") {
      e.preventDefault();
      const els = containerRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]');
      (els?.[0] as HTMLElement)?.focus();
    } else if (e.key === "End") {
      e.preventDefault();
      const els = containerRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]');
      const last = els?.[els.length - 1] as HTMLElement | undefined;
      last?.focus();
    } else if (e.key === "Tab") {
      const focusable = getFocusable();
      if (focusable.length === 0) return;
      const active = document.activeElement as HTMLElement | null;
      const idx = focusable.indexOf(active as HTMLElement);
      e.preventDefault();
      if (e.shiftKey) {
        const prev = idx <= 0 ? focusable.length - 1 : idx - 1;
        focusable[prev]?.focus();
      } else {
        const next = idx === -1 || idx >= focusable.length - 1 ? 0 : idx + 1;
        focusable[next]?.focus();
      }
    } else if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    }
  };
  return (
    <div ref={containerRef} role="menu" id="notification-panel" aria-label="Notifications" data-testid="notification-panel" onKeyDown={handleKeyDown} className={cn("absolute right-0 top-full mt-2 w-80 sm:w-96 max-h-[28rem] flex flex-col glass-menu rounded-xl shadow-2xl z-50 overflow-hidden","border border-white/10")}>
      <span tabIndex={0} aria-hidden="true" className="sr-only" onFocus={() => getFocusable().at(-1)?.focus()} />
      <div className="flex items-center justify-between px-4 py-3 border-b border-white/10 shrink-0">
        <div className="flex items-center gap-2">
          <h2 className="text-sm font-semibold text-foreground">Notifications</h2>
          {unreadCount > 0 && (<span data-testid="notification-unread-badge" className="min-w-5 h-5 px-1.5 rounded-full bg-cyan-500/20 text-cyan-300 border border-cyan-500/30 text-[11px] font-semibold flex items-center justify-center">{unreadCount}</span>)}
        </div>
        <div className="flex items-center gap-1">
          {unreadCount > 0 && (<button onClick={onMarkAllRead} className="text-xs font-medium text-cyan-400 hover:text-cyan-300 px-2 py-1 rounded hover:bg-white/10 transition focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30" aria-label="Mark all as read" data-testid="notification-mark-all-read"><span className="flex items-center gap-1"><CheckCheck aria-hidden="true" className="w-3.5 h-3.5" />Mark read</span></button>)}
          <button onClick={onClear} disabled={items.length === 0} className="text-xs font-medium text-muted-foreground hover:text-foreground px-2 py-1 rounded hover:bg-white/10 transition disabled:opacity-40 disabled:cursor-not-allowed focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30" aria-label="Clear notifications" data-testid="notification-clear">Clear</button>
          <button onClick={onClose} className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-white/10 focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30" aria-label="Close notifications"><X aria-hidden="true" className="w-4 h-4" /></button>
        </div>
      </div>
      <div ref={listRef} className="flex-1 overflow-y-auto p-1.5 space-y-1 min-h-0" style={{ scrollbarWidth: "thin" as const }}>
        {isLoading && items.length === 0 && (<div className="flex flex-col items-center justify-center py-10 text-muted-foreground gap-2"><Clock aria-hidden="true" className="w-5 h-5 animate-pulse" /><span className="text-sm">Loading events...</span></div>)}
        {isError && items.length === 0 && (<div className="flex flex-col items-center justify-center py-10 gap-3"><AlertCircle aria-hidden="true" className="w-5 h-5 text-rose-400" /><span className="text-sm text-muted-foreground">Failed to load events</span><button onClick={onRetry} className="text-xs text-cyan-400 hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30 rounded">Retry</button></div>)}
        {!isLoading && !isError && items.length === 0 && (<div className="flex flex-col items-center justify-center py-10 text-muted-foreground gap-1"><span className="text-sm font-medium">No recent events</span><span className="text-xs">Events from the last 50 actions will appear here.</span></div>)}
        {items.map((item) => {
          const kindIcon = iconForKind(item.kind);
          const fallback = iconForTargetType(item.targetType);
          const { Icon, color } = kindIcon.Icon ? kindIcon : fallback;
          return (
            <Link key={item.id} to={item.href as never} role="menuitem" tabIndex={0} data-testid="notification-item" data-unread={item.unread ? "true" : "false"} aria-label={`${item.title} — ${item.action}`} onClick={() => onItemClick(item)} onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onItemClick(item); } }} className={cn("flex items-center gap-3 px-3 py-2.5 rounded-lg border transition text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30","border-white/5 hover:bg-white/10 focus:bg-white/10",item.unread && "bg-cyan-500/5 border-cyan-500/10")}>
              <span className={cn("shrink-0 w-7 h-7 rounded-full bg-white/5 border border-white/10 flex items-center justify-center", color)}><Icon aria-hidden="true" className="w-3.5 h-3.5" /></span>
              <span className="flex-1 min-w-0"><span className="block text-sm text-foreground truncate">{item.title}</span><span className="block text-xs text-muted-foreground truncate">{item.action} • {item.targetType}</span></span>
              <span className="shrink-0 flex flex-col items-end gap-1"><span className="text-xs text-muted-foreground whitespace-nowrap">{formatRelativeTime(item.occurredAt)}</span>{item.unread && <span data-testid="notification-unread-dot" className="w-1.5 h-1.5 rounded-full bg-cyan-400" aria-hidden="true" />}</span>
            </Link>
          );
        })}
      </div>
      <div className="border-t border-white/10 p-2 shrink-0"><Link to="/admin" onClick={onClose} data-testid="notification-view-all" aria-label="View all history" className="flex items-center justify-center w-full px-3 py-2 rounded-lg text-sm font-medium text-cyan-400 hover:text-cyan-300 hover:bg-white/10 transition border border-transparent hover:border-white/5 focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30">View All History</Link></div>
      <span tabIndex={0} aria-hidden="true" className="sr-only" onFocus={() => getFocusable()[0]?.focus()} />
    </div>
  );
}
