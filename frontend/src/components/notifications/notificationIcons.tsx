import {
  PlayCircle,
  CheckCircle,
  XCircle,
  Repeat,
  Calendar,
  RotateCcw,
  CircleCheck,
  Layers,
  ShieldCheck,
  Folder,
  Container,
  Zap,
} from "lucide-react";
import type { NotificationKind } from "./useNotifications";

export function iconForKind(kind: NotificationKind): { Icon: React.ComponentType<{ className?: string }>; color: string } {
  switch (kind) {
    case "workflow.kicked":
      return { Icon: PlayCircle, color: "text-emerald-400" };
    case "workflow.finished":
      return { Icon: CheckCircle, color: "text-emerald-400" };
    case "schedule.started":
      return { Icon: Repeat, color: "text-fuchsia-400" };
    case "execution.succeeded":
      return { Icon: CheckCircle, color: "text-emerald-400" };
    case "execution.failed":
      return { Icon: XCircle, color: "text-rose-400" };
    case "recovery.triggered":
      return { Icon: RotateCcw, color: "text-rose-400" };
    case "approval.created":
    case "approval.requires_action":
      return { Icon: CircleCheck, color: "text-amber-400" };
    default:
      return { Icon: Layers, color: "text-slate-400" };
  }
}

// Export fallback icons for unknown audit actions by target_type
export function iconForTargetType(targetType: string): { Icon: React.ComponentType<{ className?: string }>; color: string } {
  switch (targetType) {
    case "workflow":
      return { Icon: PlayCircle, color: "text-sky-400" };
    case "execution":
      return { Icon: Zap, color: "text-emerald-400" };
    case "recovery":
      return { Icon: RotateCcw, color: "text-rose-400" };
    case "approval":
      return { Icon: CircleCheck, color: "text-amber-400" };
    case "work_item":
      return { Icon: CheckCircle, color: "text-indigo-400" };
    case "project":
      return { Icon: Folder, color: "text-cyan-400" };
    case "policy":
      return { Icon: ShieldCheck, color: "text-indigo-400" };
    case "runtime_image":
      return { Icon: Container, color: "text-purple-400" };
    case "schedule":
    case "recurring_schedule":
      return { Icon: Calendar, color: "text-fuchsia-400" };
    default:
      return { Icon: Layers, color: "text-slate-400" };
  }
}
