"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { PageHeader } from "@/components/ui/page-header";
import { EmptyState } from "@/components/ui/empty-state";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Stat } from "@/components/ui/stat";
import { StorageBreakdown } from "@/components/storage-breakdown";
import { QuotaRequestWidget } from "@/components/quota-request-widget";
import { ActiveOps } from "@/components/active-ops";
import { userStore, User } from "@/lib/user";
import { formatBytes } from "@/lib/format";
import {
  Database, FolderPlus, ArrowRight, HardDrive, Files, Trash2, MoreHorizontal,
  Globe, History, Archive,
} from "lucide-react";

interface Bucket {
  id: string;
  name: string;
  isPublic: boolean;
  versioning: boolean;
  archived: boolean;
  createdAt: string;
}

interface StorageEntry {
  id: string;
  name: string;
  originalBytes: number;
  transcodedBytes: number;
  reservedBytes: number;
  trashBytes: number;
  totalBytes: number;
  objectCount: number;
  archived: boolean;
}

interface Storage {
  quotaBytes: number;
  usedBytes: number;
  buckets: StorageEntry[] | null;
  trashBytes: number;
  versionsBytes: number;
  reservedBytes: number;
  totalObjectCount: number;
}

const BUCKETS_PREVIEW_LIMIT = 6;

export default function OverviewPage() {
  const [user, setUser]       = useState<User | null>(userStore.get());
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [storage, setStorage] = useState<Storage | null>(null);

  useEffect(() => userStore.subscribe(setUser), []);

  useEffect(() => {
    // Two fetches: /auth/storage for sizes+counts (also has the
    // authoritative used/quota), and / for visibility flags + createdAt.
    const load = () => {
      api<{ buckets: Bucket[] }>("/").then((r) => setBuckets(r.buckets)).catch(() => {});
      api<Storage>("/auth/storage").then(setStorage).catch(() => {});
    };
    load();
    const t = window.setInterval(load, 8000);
    return () => window.clearInterval(t);
  }, []);

  if (!user) return <p className="text-muted">Loading...</p>;

  const firstName = user.name?.split(" ")[0] || user.name || "";
  const archivedCount = buckets.filter((b) => b.archived).length;
  const publicCount   = buckets.filter((b) => b.isPublic).length;

  // Index storage by id so we can merge bytes/count into each bucket card.
  const storageById = new Map<string, StorageEntry>();
  (storage?.buckets ?? []).forEach((s) => storageById.set(s.id, s));

  const totalFiles    = storage?.totalObjectCount ?? 0;
  const usedBytes     = storage?.usedBytes        ?? user.usedBytes;
  const quotaBytes    = storage?.quotaBytes       ?? user.quotaBytes;
  const trashBytes    = storage?.trashBytes       ?? 0;
  const usedPct       = quotaBytes > 0 ? (usedBytes / quotaBytes) * 100 : 0;

  // Bucket grid: cap preview at the limit. If there are MORE than the limit,
  // we reserve the last tile for a "+N more" link so the user never thinks
  // they're seeing the whole list.
  const overflow = buckets.length > BUCKETS_PREVIEW_LIMIT;
  const visible  = overflow ? buckets.slice(0, BUCKETS_PREVIEW_LIMIT - 1) : buckets;
  const hiddenCount = overflow ? buckets.length - visible.length : 0;

  return (
    <div>
      <PageHeader
        title={<>Welcome back, <span className="text-text-soft">{firstName}</span></>}
        description={
          <>
            Signed in as <span className="font-mono text-text">{user.email}</span>
            {user.role === "admin" && <span className="ml-2"><Badge variant="accent">admin</Badge></span>}
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

      <div className="space-y-6">
        {/* At-a-glance stat row. Four small tiles for the things admins
            and users actually want to spot in one second. */}
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 sm:gap-4">
          <Card compact>
            <Stat
              size="sm"
              icon={<HardDrive size={11} />}
              label="Storage used"
              value={
                <span className={usedPct >= 90 ? "text-danger" : usedPct >= 75 ? "text-warning" : undefined}>
                  {usedPct.toFixed(0)}%
                </span>
              }
              hint={<>{formatBytes(usedBytes)} of {formatBytes(quotaBytes)}</>}
            />
          </Card>
          <Card compact>
            <Stat
              size="sm"
              icon={<Database size={11} />}
              label="Buckets"
              value={buckets.length}
              hint={
                archivedCount > 0
                  ? <>{archivedCount} archived{publicCount > 0 && <> · {publicCount} public</>}</>
                  : publicCount > 0
                    ? <>{publicCount} public</>
                    : "All active"
              }
            />
          </Card>
          <Card compact>
            <Stat
              size="sm"
              icon={<Files size={11} />}
              label="Files"
              value={totalFiles.toLocaleString()}
              hint={
                buckets.length > 0
                  ? <>across {buckets.length} {buckets.length === 1 ? "bucket" : "buckets"}</>
                  : "—"
              }
            />
          </Card>
          <Link href="/dashboard/trash" className="block group">
            <Card compact className="h-full group-hover:border-text/30 transition-colors">
              <Stat
                size="sm"
                icon={<Trash2 size={11} />}
                label="Trash"
                value={trashBytes > 0 ? formatBytes(trashBytes) : "0"}
                hint={
                  trashBytes > 0
                    ? <span className="text-link">Reclaim space →</span>
                    : "Nothing to purge"
                }
              />
            </Card>
          </Link>
        </div>

        {/* Single storage section — used %, stacked bar, per-bucket breakdown,
            quota-request CTA. No duplicate slim strip + detailed breakdown. */}
        <Card variant="elevated">
          <StorageBreakdown />
          <div className="mt-4">
            <QuotaRequestWidget />
          </div>
        </Card>

        {/* Live operations — hides when nothing's running. */}
        <ActiveOps />

        {/* Buckets — main content. */}
        <section>
          <div className="flex items-end justify-between mb-4">
            <div>
              <h2 className="text-lg font-semibold tracking-tight">Your buckets</h2>
              <p className="text-xs text-muted mt-0.5">
                {buckets.length === 0
                  ? "Where your files live."
                  : overflow
                    ? <>Showing {visible.length} of {buckets.length}</>
                    : <>{buckets.length} {buckets.length === 1 ? "container" : "containers"}</>}
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
              {visible.map((b) => (
                <BucketTile key={b.id} bucket={b} stats={storageById.get(b.id)} />
              ))}
              {overflow && <MoreBucketsTile count={hiddenCount} />}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

function BucketTile({ bucket: b, stats }: { bucket: Bucket; stats: StorageEntry | undefined }) {
  // Live bytes only — exclude trash so the "occupied" number matches what
  // a normal `ls` would show. trash is counted separately in the top stats.
  const liveBytes  = stats ? stats.originalBytes + stats.transcodedBytes + stats.reservedBytes : 0;
  const fileCount  = stats?.objectCount ?? 0;
  return (
    <Link href={`/dashboard/buckets/${b.name}`} className="group">
      <Card className={`h-full hover:border-text/30 hover:shadow-elevated transition-all ${b.archived ? "opacity-70" : ""}`}>
        <div className="flex items-start gap-3 mb-3">
          <div className="w-9 h-9 rounded-lg bg-surface flex items-center justify-center text-muted shrink-0 group-hover:text-text transition-colors">
            <Database size={16} />
          </div>
          <div className="min-w-0 flex-1">
            <h3 className="text-sm font-semibold tracking-tight truncate group-hover:text-link transition-colors">
              {b.name}
            </h3>
            <p className="text-[11px] text-muted mt-0.5">
              {fileCount === 0
                ? "Empty"
                : `${fileCount.toLocaleString()} ${fileCount === 1 ? "file" : "files"}`}
              {" · "}{formatBytes(liveBytes)}
            </p>
          </div>
          <ArrowRight size={14} className="text-muted shrink-0 group-hover:text-text group-hover:translate-x-0.5 transition-all" />
        </div>

        {/* Status badges — only render when there's something noteworthy.
            Default private/single-version buckets stay clean. */}
        {(b.isPublic || b.versioning || b.archived) && (
          <div className="flex flex-wrap gap-1 mb-2">
            {b.isPublic
              ? <Badge variant="warning"><Globe size={9} /> Public</Badge>
              : null}
            {b.versioning && <Badge variant="neutral"><History size={9} /> Versioned</Badge>}
            {b.archived  && <Badge variant="neutral"><Archive size={9} /> Archived</Badge>}
          </div>
        )}

        {/* Created date — quietest line, gives history without dominating */}
        <p className="text-[10px] text-muted">
          Created {new Date(b.createdAt).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })}
        </p>
      </Card>
    </Link>
  );
}

function MoreBucketsTile({ count }: { count: number }) {
  return (
    <Link href="/dashboard/buckets" className="group">
      <Card className="h-full border-dashed hover:border-text/40 transition-colors flex items-center justify-center min-h-[140px]">
        <div className="text-center">
          <div className="w-9 h-9 rounded-lg bg-surface flex items-center justify-center text-muted mx-auto mb-2 group-hover:text-text transition-colors">
            <MoreHorizontal size={16} />
          </div>
          <p className="text-sm font-medium group-hover:text-link transition-colors">
            +{count} more
          </p>
          <p className="text-[11px] text-muted mt-0.5">View all buckets</p>
        </div>
      </Card>
    </Link>
  );
}
