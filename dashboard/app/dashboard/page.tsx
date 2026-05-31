"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { PageHeader } from "@/components/ui/page-header";
import { SectionHeader } from "@/components/ui/section";
import { EmptyState } from "@/components/ui/empty-state";
import { Stat } from "@/components/ui/stat";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StorageBreakdown } from "@/components/storage-breakdown";
import { QuotaRequestWidget } from "@/components/quota-request-widget";
import { ActiveOps } from "@/components/active-ops";
import { Database, FolderPlus, ArrowRight, HardDrive, Layers } from "lucide-react";

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

  const usagePct = me.quotaBytes > 0 ? Math.min(100, Math.round((me.usedBytes / me.quotaBytes) * 100)) : 0;
  const firstName = me.name?.split(" ")[0] || me.name || "";

  return (
    <div>
      <PageHeader
        title={<>Welcome back, <span className="text-text-soft">{firstName}</span></>}
        description={
          <>
            Signed in as <span className="font-mono text-text">{me.email}</span>
            {me.role === "admin" && <span className="ml-2"><Badge variant="accent">admin</Badge></span>}
          </>
        }
        actions={
          <Link href="/dashboard/buckets">
            <Button variant="primary">
              <FolderPlus size={14} />
              New bucket
            </Button>
          </Link>
        }
      />

      {/* Stats row — at-a-glance numbers above the fold. */}
      <div className="grid grid-cols-2 sm:grid-cols-3 gap-3 sm:gap-4 mb-6">
        <Card>
          <Stat
            label="Storage used"
            value={`${usagePct}%`}
            hint={`of your quota`}
            icon={<HardDrive size={11} />}
          />
        </Card>
        <Card>
          <Stat
            label="Buckets"
            value={buckets.length}
            hint={buckets.length === 1 ? "1 container" : `${buckets.length} containers`}
            icon={<Database size={11} />}
          />
        </Card>
        <Card className="col-span-2 sm:col-span-1">
          <Stat
            label="Plan"
            value={
              <span className="capitalize">{me.role === "admin" ? "Administrator" : "User"}</span>
            }
            hint={me.role === "admin" ? "Full system access" : "Standard access"}
            icon={<Layers size={11} />}
          />
        </Card>
      </div>

      {/* Live operations (uploads, imports, transcodes). Hides itself when nothing's running. */}
      <ActiveOps />

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4 sm:gap-6 mt-6">
        {/* Storage breakdown — the hero card on this page. */}
        <Card variant="elevated" className="lg:col-span-2">
          <SectionHeader
            title="Storage usage"
            description="What's living in each bucket, including transcoded variants and trash."
          />
          <StorageBreakdown />
          <div className="mt-4">
            <QuotaRequestWidget />
          </div>
        </Card>

        {/* Buckets shortcut. */}
        <Card>
          <SectionHeader
            title="Your buckets"
            actions={
              <Link href="/dashboard/buckets" className="text-xs text-link hover:underline inline-flex items-center gap-0.5">
                View all <ArrowRight size={11} />
              </Link>
            }
          />
          {buckets.length === 0 ? (
            <EmptyState
              compact
              icon={<Database size={18} />}
              title="No buckets yet"
              description="Create one to start uploading files."
              action={
                <Link href="/dashboard/buckets">
                  <Button size="sm">Go to Buckets</Button>
                </Link>
              }
            />
          ) : (
            <ul className="-mx-2">
              {buckets.slice(0, 8).map((b) => (
                <li key={b.id}>
                  <Link
                    href={`/dashboard/buckets/${b.name}`}
                    className="flex items-center gap-2.5 px-2 py-1.5 rounded-md text-sm text-text-soft hover:text-text hover:bg-surface transition-colors"
                  >
                    <Database size={14} className="text-muted shrink-0" />
                    <span className="truncate font-medium">{b.name}</span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
