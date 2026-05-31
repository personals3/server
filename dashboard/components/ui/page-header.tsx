import { ReactNode } from "react";
import { cn } from "@/lib/format";

interface PageHeaderProps {
  title: ReactNode;
  description?: ReactNode;
  /** Right-aligned action area (buttons, dropdowns). */
  actions?: ReactNode;
  /** Small label that appears ABOVE the title — e.g. "Buckets" → "my-bucket" */
  eyebrow?: ReactNode;
  className?: string;
}

/**
 * Top of every page: eyebrow label + display-size title + soft subtitle +
 * trailing actions. Use this instead of rolling your own h1/p combo so
 * every page has the same vertical rhythm.
 */
export function PageHeader({ title, description, actions, eyebrow, className }: PageHeaderProps) {
  return (
    <header className={cn("flex flex-wrap items-end justify-between gap-4 mb-8", className)}>
      <div className="min-w-0">
        {eyebrow && (
          <p className="text-[11px] uppercase tracking-[0.15em] text-muted mb-2">{eyebrow}</p>
        )}
        <h1 className="text-display-sm font-semibold tracking-tight text-text leading-tight">
          {title}
        </h1>
        {description && (
          <p className="text-sm text-text-soft mt-2 max-w-2xl">{description}</p>
        )}
      </div>
      {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
    </header>
  );
}
