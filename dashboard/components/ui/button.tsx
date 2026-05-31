import { cn } from "@/lib/format";
import { ButtonHTMLAttributes, forwardRef } from "react";

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "primary" | "secondary" | "danger" | "ghost" | "link";
  size?: "sm" | "md" | "lg";
}

/**
 * Canonical button.
 *
 * Visual hierarchy:
 *   primary   — high-contrast monochrome (text-on-bg) — the action of the screen
 *   secondary — outlined neutral — alternative paths
 *   danger    — destructive
 *   ghost     — transparent until hover — toolbar / table-row affordances
 *   link      — looks like text + underline-on-hover — inline actions
 *
 * Use exactly one primary per screen. Two primaries fight each other.
 */
export const Button = forwardRef<HTMLButtonElement, Props>(
  ({ variant = "primary", size = "md", className, ...rest }, ref) => {
    const base = cn(
      "inline-flex items-center justify-center gap-1.5",
      "font-medium rounded-md transition-colors",
      "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-text/40 focus-visible:ring-offset-2 focus-visible:ring-offset-bg",
      "disabled:opacity-50 disabled:cursor-not-allowed",
    );
    const sizes = {
      sm: "h-7 px-2.5 text-xs",
      md: "h-9 px-3.5 text-sm",
      lg: "h-11 px-5 text-[15px]",
    }[size];
    const variants = {
      // text-on-bg = near-black on light, near-white on dark — clean monochrome
      primary:   "bg-text text-bg hover:opacity-90 active:opacity-80",
      secondary: "bg-transparent border border-border hover:border-text/40 hover:bg-surface text-text",
      danger:    "bg-danger text-white hover:opacity-90 active:opacity-80",
      ghost:     "bg-transparent hover:bg-surface text-text-soft hover:text-text",
      link:      "bg-transparent text-link hover:underline underline-offset-2 px-0 h-auto",
    }[variant];
    return <button ref={ref} className={cn(base, sizes, variants, className)} {...rest} />;
  },
);
Button.displayName = "Button";
