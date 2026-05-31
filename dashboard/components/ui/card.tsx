import { cn } from "@/lib/format";
import { HTMLAttributes } from "react";

export function Card({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn(
        "bg-panel border border-border rounded-lg p-5",
        "shadow-[0_1px_0_0_rgba(255,255,255,0.02)_inset]",
        className,
      )}
      {...rest}
    />
  );
}
