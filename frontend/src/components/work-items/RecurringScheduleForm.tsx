import { useEffect, useState } from "react";
import { RecurringSchedule } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

const FREQUENCIES = [
  { value: "minute", label: "Minute" },
  { value: "hourly", label: "Hourly" },
  { value: "daily", label: "Daily" },
  { value: "weekly", label: "Weekly" },
  { value: "monthly", label: "Monthly" },
] as const;

const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"] as const;

export interface RecurringScheduleFormProps {
  value: RecurringSchedule | undefined;
  onChange: (schedule: RecurringSchedule | undefined) => void;
  disabled?: boolean;
  readOnly?: boolean;
}

function isSchedulePopulated(s: RecurringSchedule | undefined): boolean {
  if (!s) return false;
  return !!s.frequency || s.interval > 0 || s.days.length > 0 || !!s.startDate || !!s.startTime;
}

export function RecurringScheduleForm({
  value,
  onChange,
  disabled = false,
  readOnly = false,
}: RecurringScheduleFormProps) {
  const [enabled, setEnabled] = useState(isSchedulePopulated(value));

  // Sync checkbox with value content — an empty proto RecurringSchedule
  // (emitted on uncheck) is truthy but should show as disabled.
  useEffect(() => {
    setEnabled(isSchedulePopulated(value));
  }, [value]);

  const frequency = value?.frequency ?? "daily";
  const interval = value?.interval ?? 1;
  const days = value?.days ?? [];
  const startDate = value?.startDate ?? "";
  const startTime = value?.startTime ?? "";

  const emit = (patch: Partial<RecurringSchedule>) => {
    const base = value ?? new RecurringSchedule();
    const next = new RecurringSchedule({
      ...base,
      ...patch,
    });
    onChange(next);
  };

  const handleToggle = (on: boolean) => {
    setEnabled(on);
    if (!on) {
      // Emit an empty message to signal "clear" — proto3 optional presence
      // means undefined = unchanged, empty message = clear.
      onChange(new RecurringSchedule());
    } else {
      // Default: daily, interval 1, start_date = today
      const today = new Date();
      const yyyy = today.getFullYear();
      const mm = String(today.getMonth() + 1).padStart(2, "0");
      const dd = String(today.getDate()).padStart(2, "0");
      onChange(
        new RecurringSchedule({
          frequency: "daily",
          interval: 1,
          days: [],
          startDate: `${yyyy}-${mm}-${dd}`,
          startTime: "09:00",
        }),
      );
    }
  };

  if (readOnly && !value) return null;

  return (
    <div className="space-y-3">
      {!readOnly && (
        <div className="flex items-center gap-2">
          <input
            type="checkbox"
            id="recurringEnabled"
            checked={enabled}
            onChange={(e) => handleToggle(e.target.checked)}
            disabled={disabled}
            className="h-4 w-4 rounded border-input"
          />
          <Label htmlFor="recurringEnabled" className="font-medium">
            Recurring schedule
          </Label>
        </div>
      )}

      {enabled && value && (
        <div className="space-y-3 rounded-md border bg-muted/30 p-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="recurringFrequency">Frequency</Label>
              <select
                id="recurringFrequency"
                value={frequency}
                onChange={(e) => emit({ frequency: e.target.value })}
                disabled={disabled}
                className="flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm"
              >
                {FREQUENCIES.map((f) => (
                  <option key={f.value} value={f.value}>
                    {f.label}
                  </option>
                ))}
              </select>
            </div>

            <div className="space-y-1">
              <Label htmlFor="recurringInterval">Every</Label>
              <div className="flex items-center gap-2">
                <Input
                  id="recurringInterval"
                  type="number"
                  min={1}
                  max={365}
                  value={interval}
                  onChange={(e) =>
                    emit({ interval: Math.max(1, Number(e.target.value) || 1) })
                  }
                  disabled={disabled}
                  className="h-9 w-20"
                />
                <span className="text-sm text-muted-foreground">
                  {frequency === "minute"
                    ? "minute(s)"
                    : frequency === "hourly"
                      ? "hour(s)"
                      : frequency === "daily"
                        ? "day(s)"
                        : frequency === "weekly"
                          ? "week(s)"
                          : "month(s)"}
                </span>
              </div>
            </div>
          </div>

          {frequency === "weekly" && (
            <div className="space-y-1">
              <Label>Days of week</Label>
              <div className="flex flex-wrap gap-1">
                {DAYS.map((day) => {
                  const isActive = days.includes(day);
                  return (
                    <button
                      key={day}
                      type="button"
                      disabled={disabled}
                      onClick={() => {
                        const next = isActive
                          ? days.filter((d) => d !== day)
                          : [...days, day];
                        emit({ days: next });
                      }}
                      className={cn(
                        "rounded-md border px-2.5 py-1 text-xs font-medium transition-colors",
                        isActive
                          ? "border-primary bg-primary text-primary-foreground"
                          : "border-input bg-background text-muted-foreground hover:bg-accent hover:text-accent-foreground",
                        disabled && "opacity-50 cursor-not-allowed",
                      )}
                    >
                      {day}
                    </button>
                  );
                })}
              </div>
              {days.length === 0 && (
                <p className="text-xs text-muted-foreground">
                  No days selected — runs every day.
                </p>
              )}
            </div>
          )}

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="recurringStartDate">Start date</Label>
              <Input
                id="recurringStartDate"
                type="date"
                value={startDate}
                onChange={(e) => emit({ startDate: e.target.value })}
                disabled={disabled}
                className="h-9"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="recurringStartTime">Start time</Label>
              <Input
                id="recurringStartTime"
                type="time"
                value={startTime}
                onChange={(e) => emit({ startTime: e.target.value })}
                disabled={disabled}
                className="h-9"
              />
            </div>
          </div>
        </div>
      )}

      {readOnly && value && (
        <div className="text-sm text-muted-foreground">
          <span className="font-medium capitalize">{value.frequency}</span>
          {value.interval > 1 && ` every ${value.interval}`}
          {value.days.length > 0 && ` (${value.days.join(", ")})`}
          {value.startDate && ` starting ${value.startDate}`}
          {value.startTime && ` at ${value.startTime}`}
        </div>
      )}
    </div>
  );
}

/**
 * Format a RecurringSchedule into a human-readable badge string.
 * Used by the schedules page recurrenceBadge() stub.
 */
export function formatRecurrence(schedule: RecurringSchedule | undefined): string {
  if (!isSchedulePopulated(schedule)) return "One-time";
  const parts: string[] = [];
  parts.push(schedule.frequency);
  if (schedule.interval > 1) parts.push(`every ${schedule.interval}`);
  if (schedule.days.length > 0) parts.push(`(${schedule.days.join(", ")})`);
  return parts.join(" ");
}
