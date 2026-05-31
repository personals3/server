"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { PageHeader } from "@/components/ui/page-header";
import { EmptyState } from "@/components/ui/empty-state";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { StorageBreakdown } from "@/components/storage-breakdown";
import { QuotaRequestWidget } from "@/components/quota-request-widget";
import { ActiveOps } from "@/components/active-ops";
import { Database, FolderPlus, ArrowRight } from "lucide-react";

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
    const t = window.setInterval(load, 8000);
    return () => window.clearInterval(t);
  }, []);

  if (!me) return <p className="text-muted">Loading...</p>;

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
          <Link href="/dashboard/buckets?create=1">
            <Button variant="primary">
              <FolderPlus size={14} />
              New bucket
            </Button>
          </Link>
        }
      />

      {/* Single storage section — used %, stacked bar, per-bucket breakdown,
          quota-request CTA. No duplicate slim strip + detailed breakdown. */}
      <Card variant="elevated" className="mb-6">
        <StorageBreakdown />
        <div className="mt-4">
          <QuotaRequestWidget />
        </div>
      </Card>

      {/* Live operations — hides when nothing's running. */}
      <ActiveOps />

      {/* Buckets — main content. */}
      <section className="mt-6">
        <div className="flex items-end justify-between mb-4">
          <div>
            <h2 className="text-lg font-semibold tracking-tight">Your buckets</h2>
            <p className="text-xs text-muted mt-0.5">
              {buckets.length === 0 ? "Where your files live." : `${buckets.length} ${buckets.length === 1 ? "container" : "containers"}`}
            </p>
          </div>
          {buckets.length > 0 && (
            <Link
              href="/dashboard/buckets"
              className="text-xs text-link hover:underline inline-flex items-center gap-0.5"
            >
              Manage all <ArrowRight size={11} />
            </Link>
          )}
        </div>

        {buckets.length === 0 ? (
          <Card>
            <EmptyState
              icon={<Database size={22} />}
              title="No buckets yet"
              description="Buckets are isolated containers for your files. Create one to start uploading."
              action={
                <Link href="/dashboard/buckets?create=1">
                  <Button><FolderPlus size={14} />Create your first bucket</Button>
                </Link>
              }
            />
          </Card>
        ) : (
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3 sm:gap-4">
            {buckets.slice(0, 8).map((b) => (
              <Link
                key={b.id}
                href={`/dashboard/buckets/${b.name}`}
                className="group"
              >
                <Card className="h-full hover:border-text/30 hover:shadow-elevated transition-all">
                  <div className="flex items-start gap-3">
                    <div className="w-9 h-9 rounded-lg bg-surface flex items-center justify-center text-muted shrink-0 group-hover:text-text transition-colors">
                      <Database size={16} />
                    </div>
                    <div className="min-w-0 flex-1">
                      <h3 className="text-sm font-semibold tracking-tight truncate group-hover:text-link transition-colors">
                        {b.name}
                      </h3>
                      <p className="text-[11px] text-muted mt-0.5">
                        Created {new Date(b.createdAt).toLocaleDateString(undefined, { month: "short", day: "numeric" })}
                      </p>
                    </div>
                    <ArrowRight size={14} className="text-muted shrink-0 group-hover:text-text group-hover:translate-x-0.5 transition-all" />
                  </div>
                </Card>
              </Link>
            ))}
            <Link href="/dashboard/buckets?create=1" className="group">
              <Card className="h-full border-dashed hover:border-text/40 transition-colors flex items-center justify-center">
                <div className="flex items-center gap-2 text-sm text-muted group-hover:text-text transition-colors">
                  <FolderPlus size={16} />
                  New bucket
                </div>
              </Card>
            </Link>
          </div>
        )}
      </section>
    </div>
  );
}
