import { formatBytes } from "@/lib/format";

interface Props {
  used: number;
  total: number;
}

export function QuotaBar({ used, total }: Props) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  const color =
    pct < 70 ? "bg-success" : pct < 90 ? "bg-warning" : "bg-danger";
  return (
    <div className="space-y-1">
      <div className="flex justify-between text-xs text-muted">
        <span>{formatBytes(used)} used</span>
        <span>{formatBytes(total)} quota ({pct.toFixed(1)}%)</span>
      </div>
      <div className="h-2 w-full bg-border rounded overflow-hidden">
        <div className={`h-full ${color} transition-all`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}
