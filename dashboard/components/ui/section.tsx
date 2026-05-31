import { ReactNode } from "react";
import { cn } from "@/lib/format";

interface SectionHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  className?: string;
}

/**
 * Sub-page section divider — smaller than PageHeader. Use inside a page
 * to label a Card's content or group related blocks.
 */
export function SectionHeader({ title, description, actions, className }: SectionHeaderProps) {
  return (
    <div className={cn("flex items-end justify-between gap-3 mb-4", className)}>
      <div className="min-w-0">
        <h2 className="text-sm font-semibold text-text tracking-tight">{title}</h2>
        {description && <p className="text-xs text-muted mt-0.5">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-1.5 shrink-0">{actions}</div>}
    </div>
  );
}
