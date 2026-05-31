"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { StorageBreakdown } from "@/components/storage-breakdown";
import { QuotaRequestWidget } from "@/components/quota-request-widget";
import { ActiveOps } from "@/components/active-ops";
import { Database } from "lucide-react";
import Link from "next/link";

interface Me {
  id: string; email: string; name: string; role: string;
  quotaBytes: number; usedBytes: number;
}
interface Bucket { id: string; name: string; createdAt: string; }

export default function OverviewPage() {
  const [me, setMe] = useState<Me | null>(null);
  const [buckets, setBuckets] = useState<Bucket[]>([]);

  useEffect(() => {
    const load = () => {
      api<Me>("/auth/me").then(setMe).catch(() => {});
      api<{ buckets: Bucket[] }>("/").then((r) => setBuckets(r.buckets)).catch(() => {});
    };
    load();
    // Refresh storage usage every 8s so the bar reflects transcode publishes
    // and uploads without a manual reload.
    const t = window.setInterval(load, 8000);
    return () => window.clearInterval(t);
  }, []);

  if (!me) return <p className="text-muted">Loading...</p>;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Welcome, {me.name}</h1>
        <p className="text-muted text-sm truncate" title={me.email}>
          Signed in as <span className="font-mono">{me.email}</span>
          {me.role === "admin" && (
            <span className="ml-2 text-xs bg-accent px-2 py-0.5 rounded text-white">admin</span>
          )}
        </p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <Card className="lg:col-span-2 space-y-4">
          <h2 className="text-sm font-semibold">Storage usage</h2>
          <StorageBreakdown />
          <QuotaRequestWidget />
        </Card>
        <Card>
          <h2 className="text-sm font-semibold mb-3">Quick stats</h2>
          <div className="space-y-1 text-sm">
            <div className="flex justify-between"><span className="text-muted">Buckets</span><span>{buckets.length}</span></div>
            <div className="flex justify-between"><span className="text-muted">Role</span><span className="font-mono">{me.role}</span></div>
          </div>
        </Card>
      </div>

      {/* "What's running RIGHT NOW" — uploads (server-side state), imports, transcodes.
          Hides itself when nothing's happening. */}
      <ActiveOps />

      <Card>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-semibold">Your buckets</h2>
          <Link href="/dashboard/buckets" className="text-xs text-accent hover:underline">
            Manage →
          </Link>
        </div>
        {buckets.length === 0 ? (
          <p className="text-muted text-sm">No buckets yet — go to Buckets to create one.</p>
        ) : (
          <ul className="space-y-2">
            {buckets.slice(0, 6).map((b) => (
              <li key={b.id}>
                <Link
                  href={`/dashboard/buckets/${b.name}`}
                  className="flex items-center gap-2 text-sm hover:text-accent"
                >
                  <Database size={14} className="text-muted" />
                  {b.name}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}
