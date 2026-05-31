import { ReactNode } from "react";
import { cn } from "@/lib/format";

interface StatProps {
  label: ReactNode;
  value: ReactNode;
  /** Optional small caption under the value (e.g. "of 1 TB", "+12 this week"). */
  hint?: ReactNode;
  /** Optional icon shown small + muted to the left of the label. */
  icon?: ReactNode;
  /** Larger size for hero metrics on the dashboard home. */
  size?: "sm" | "md" | "lg";
  className?: string;
}

/**
 * Big-number metric. Bigger than body text, smaller than a display heading.
 * Use for "Buckets: 3", "Used: 169 MB / 704 MB" etc.
 */
export function Stat({ label, value, hint, icon, size = "md", className }: StatProps) {
  const valueClass = {
    sm: "text-xl font-semibold",
    md: "text-2xl font-semibold tracking-tight",
    lg: "text-3xl font-semibold tracking-tight",
  }[size];
  return (
    <div className={cn("min-w-0", className)}>
      <div className="flex items-center gap-1.5 text-xs text-muted mb-1">
        {icon && <span className="shrink-0">{icon}</span>}
        <span className="uppercase tracking-wider">{label}</span>
      </div>
      <div className={cn(valueClass, "text-text")}>{value}</div>
      {hint && <div className="text-xs text-muted mt-1">{hint}</div>}
    </div>
  );
}
