import { ReactNode } from "react";
import { cn } from "@/lib/format";

interface BadgeProps {
  children: ReactNode;
  variant?: "neutral" | "success" | "warning" | "danger" | "accent";
  size?: "sm" | "md";
  className?: string;
}

/**
 * Small inline label. Used for role tags ("admin"), status pills,
 * "BETA" markers etc. Variants share the same shape; colour conveys
 * meaning. Default is `neutral` (low-contrast — does not draw attention).
 */
export function Badge({ children, variant = "neutral", size = "sm", className }: BadgeProps) {
  const sizing = {
    sm: "text-[10px] px-1.5 py-0.5",
    md: "text-xs px-2 py-0.5",
  }[size];
  const styles = {
    neutral: "bg-surface text-text-soft border border-border-subtle",
    success: "bg-success/10 text-success border border-success/20",
    warning: "bg-warning/10 text-warning border border-warning/20",
    danger:  "bg-danger/10 text-danger border border-danger/20",
    accent:  "bg-text text-bg",
  }[variant];
  return (
    <span className={cn(
      "inline-flex items-center gap-1 font-medium rounded-md uppercase tracking-wider whitespace-nowrap",
      sizing,
      styles,
      className,
    )}>
      {children}
    </span>
  );
}
