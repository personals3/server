import { cn } from "@/lib/format";
import { InputHTMLAttributes, forwardRef } from "react";

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...rest }, ref) => (
    <input
      ref={ref}
      className={cn(
        "w-full px-3 py-2 bg-panel border border-border rounded text-text text-sm",
        "focus:outline-none focus:border-accent",
        className,
      )}
      {...rest}
    />
  ),
);
Input.displayName = "Input";
