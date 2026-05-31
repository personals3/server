import { cn } from "@/lib/format";
import { HTMLAttributes } from "react";

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  variant?: "default" | "elevated" | "ghost";
  /** Smaller padding for tighter contexts (sidebar widgets, list items). */
  compact?: boolean;
}

/**
 * The dashboard's canonical container.
 *
 *   default   — quiet bordered panel (most cards on the page)
 *   elevated  — slightly raised, used for the most-important block per screen
 *   ghost     — borderless, padding only — for groups where the bg already breaks
 *
 * Compact halves the vertical padding for dense lists.
 */
export function Card({ className, variant = "default", compact, ...rest }: CardProps) {
  const base = cn(
    "rounded-xl",
    compact ? "p-3 sm:p-4" : "p-5 sm:p-6",
  );
  const styles = {
    default:  "bg-panel border border-border-subtle",
    elevated: "bg-panel border border-border shadow-elevated",
    ghost:    "bg-transparent",
  }[variant];
  return <div className={cn(base, styles, className)} {...rest} />;
}
