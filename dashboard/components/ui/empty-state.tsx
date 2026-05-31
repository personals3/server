import { ReactNode } from "react";
import { cn } from "@/lib/format";

interface EmptyStateProps {
  /** A Lucide icon (or any ReactNode). Rendered centered above the title. */
  icon?: ReactNode;
  title: ReactNode;
  description?: ReactNode;
  action?: ReactNode;
  className?: string;
  /** Smaller variant for empty cards inside grids. */
  compact?: boolean;
}

/**
 * Empty / first-run state. Centered icon + helpful copy + a CTA.
 * Replaces ad-hoc "No buckets yet — go to Buckets to create one." lines.
 */
export function EmptyState({ icon, title, description, action, className, compact }: EmptyStateProps) {
  return (
    <div className={cn(
      "flex flex-col items-center text-center",
      compact ? "py-6 px-4 gap-2" : "py-12 px-6 gap-3",
      className,
    )}>
      {icon && (
        <div className={cn(
          "inline-flex items-center justify-center rounded-full bg-surface text-muted",
          compact ? "w-9 h-9" : "w-12 h-12",
        )}>
          {icon}
        </div>
      )}
      <h3 className={cn(
        "font-semibold text-text",
        compact ? "text-sm" : "text-base",
      )}>{title}</h3>
      {description && (
        <p className={cn(
          "text-text-soft max-w-md",
          compact ? "text-xs" : "text-sm",
        )}>{description}</p>
      )}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}
