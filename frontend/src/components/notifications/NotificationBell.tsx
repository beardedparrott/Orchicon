import { useEffect, useRef, useState } from "react";
import { Bell } from "lucide-react";
import { useNotifications } from "./useNotifications";
import { NotificationPanel } from "./NotificationPanel";

export function NotificationBell() {
  const { items, unreadCount, isLoading, isError, refetch, markItemRead, markAllRead, clearAll } = useNotifications();
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  // Click outside + ESC to close; restore focus to trigger.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  const handleToggle = () => setOpen((v) => !v);
  const handleClose = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };

  const handleItemClick = (item: Parameters<typeof markItemRead>[0]) => {
    markItemRead(item);
    setOpen(false);
  };

  const handleMarkAll = () => {
    markAllRead();
  };

  const handleClear = () => {
    clearAll();
  };

  return (
    <div ref={containerRef} className="relative">
      <button
        ref={triggerRef}
        onClick={handleToggle}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls="notification-panel"
        aria-label={unreadCount > 0 ? `Notifications — ${unreadCount} unread` : "Notifications"}
        title="Notifications"
        data-testid="notification-bell"
        className="p-2 text-slate-400 hover:text-white rounded-lg hover:bg-white/5 transition relative focus:outline-none focus-visible:ring-2 focus-visible:ring-cyan-400/30"
      >
        <Bell aria-hidden="true" className="w-4 h-4" />
        {unreadCount > 0 && (
          <span
            data-testid="notification-pulsing-dot"
            className="absolute top-1.5 right-1.5 w-2 h-2 rounded-full bg-cyan-400 animate-pulse"
            aria-hidden="true"
          />
        )}
      </button>
      {open && (
        <NotificationPanel
          open={open}
          items={items}
          unreadCount={unreadCount}
          isLoading={isLoading}
          isError={isError}
          onClose={handleClose}
          onMarkAllRead={handleMarkAll}
          onClear={handleClear}
          onItemClick={handleItemClick}
          onRetry={() => refetch()}
        />
      )}
    </div>
  );
}
