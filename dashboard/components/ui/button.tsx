import { cn } from "@/lib/format";
import { ButtonHTMLAttributes, forwardRef } from "react";

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "primary" | "secondary" | "danger" | "ghost";
  size?: "sm" | "md";
}

export const Button = forwardRef<HTMLButtonElement, Props>(
  ({ variant = "primary", size = "md", className, ...rest }, ref) => {
    const base = "inline-flex items-center justify-center font-medium rounded transition disabled:opacity-50 disabled:cursor-not-allowed";
    const sizes = size === "sm" ? "px-3 py-1.5 text-sm" : "px-4 py-2 text-sm";
    const variants = {
      primary:   "bg-accent hover:bg-blue-600 text-white",
      secondary: "bg-panel border border-border hover:border-muted text-text",
      danger:    "bg-danger hover:bg-red-600 text-white",
      ghost:     "hover:bg-panel text-text",
    }[variant];
    return <button ref={ref} className={cn(base, sizes, variants, className)} {...rest} />;
  },
);
Button.displayName = "Button";
