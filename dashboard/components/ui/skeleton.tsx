import { cn } from "@/lib/format";

/** A single shimmer block. Compose these to mimic the shape of whatever is
 *  loading so the layout doesn't jump when real content arrives. */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("animate-pulse rounded bg-surface", className)} aria-hidden />;
}

/**
 * Placeholder rows for a list/table that's still loading. Renders inside the
 * same container as the real rows so there's no empty-state flash and no
 * height jump. Mark the region busy for screen readers via aria-busy on the
 * parent — callers using <table> should pass `as="tr"`-style via `row`.
 */
export function ListSkeleton({
  rows = 6,
  className,
}: {
  rows?: number;
  className?: string;
}) {
  return (
    <div className={cn("space-y-2 py-1", className)} aria-busy="true" aria-live="polite">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-3">
          <Skeleton className="h-7 w-7 shrink-0 rounded-md" />
          <Skeleton className={cn("h-3.5", i % 3 === 0 ? "w-1/2" : i % 3 === 1 ? "w-2/3" : "w-2/5")} />
          <Skeleton className="h-3 w-14 shrink-0 ml-auto" />
          <Skeleton className="h-3 w-24 shrink-0 hidden sm:block" />
        </div>
      ))}
    </div>
  );
}
