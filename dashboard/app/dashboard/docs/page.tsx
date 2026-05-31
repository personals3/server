"use client";

// Docs reader — editorial layout.
//
// Design intent:
//   - Wide breathing space, no "fighting for the same pixel"
//   - 3-column on wide screens: nav (260px) · reader (max 44rem) · ToC (220px)
//   - Generous typography: 17px body, 2.5rem h1, 1.75 line-height
//   - Single accent color, warm-stone palette, dark/light parity
//   - When no doc is selected we show a real landing page, not a panel
//     full of dense cards — large hero, three or four pillars
//   - Sidebar collapses on narrow screens into an off-canvas drawer

import { useEffect, useMemo, useState } from "react";
import { api } from "@/lib/api";
import { renderMarkdown, MarkdownResult, TocEntry } from "@/lib/markdown";
import { ThemeToggle } from "@/components/theme-toggle";
import {
  BookOpen, Search, ExternalLink, ArrowLeft, ArrowRight,
  Menu, X, Upload, Share2, FolderTree, Key, KeyRound, Database,
  PlayCircle, Wallet, LifeBuoy,
} from "lucide-react";
import Link from "next/link";

interface DocEntry {
  slug: string;
  title: string;
  summary: string;
  section: string;
  url?: string;      // absolute URL to raw .md on the public bucket
  pageUrl?: string;  // absolute URL to the standalone HTML viewer
}
interface DocsListResp {
  docs: DocEntry[];
  source: "bucket" | "disk";
  bucket?: string;
}

const SECTION_LABEL: Record<string, string> = {
  "":               "Get started",
  account:          "Account & access",
  uploading:        "Uploading files",
  files:            "Working with files",
  streaming:        "Streaming media",
  quotas:           "Quotas",
  troubleshooting:  "Troubleshooting",
};
const SECTION_ORDER = ["", "account", "uploading", "files", "streaming", "quotas", "troubleshooting"];

export default function DocsPage() {
  const [resp, setResp] = useState<DocsListResp | null>(null);
  const [filter, setFilter] = useState("");
  const [selectedSlug, setSelectedSlug] = useState<string | null>(null);
  const [parsed, setParsed] = useState<MarkdownResult>({ html: "", toc: [] });
  const [loading, setLoading] = useState(false);
  const [navOpen, setNavOpen] = useState(false);

  useEffect(() => {
    void api<DocsListResp>("/docs").then(setResp).catch(() => setResp({ docs: [], source: "disk" }));
  }, []);

  const selected = selectedSlug && resp?.docs.find((d) => d.slug === selectedSlug);

  useEffect(() => {
    if (!selected) { setParsed({ html: "", toc: [] }); return; }
    setLoading(true);
    const target = selected.url || `/api/docs/${selected.slug}`;
    void fetch(target, { credentials: "omit" })
      .then((r) => r.text())
      .then((text) => setParsed(renderMarkdown(text)))
      .catch(() => setParsed({ html: "<p>Failed to load.</p>", toc: [] }))
      .finally(() => {
        setLoading(false);
        window.scrollTo({ top: 0, behavior: "smooth" });
      });
  }, [selected]);

  const grouped = useMemo(() => {
    if (!resp) return {};
    const q = filter.trim().toLowerCase();
    const list = q
      ? resp.docs.filter((d) =>
          d.title.toLowerCase().includes(q) ||
          d.summary.toLowerCase().includes(q) ||
          d.slug.toLowerCase().includes(q))
      : resp.docs;
    const out: Record<string, DocEntry[]> = {};
    list.forEach((d) => { out[d.section] ||= []; out[d.section].push(d); });
    return out;
  }, [resp, filter]);

  // Adjacent navigation for the bottom of an open doc
  const flatOrder = useMemo(() => {
    if (!resp) return [];
    return SECTION_ORDER.flatMap((s) => grouped[s] || []);
  }, [resp, grouped]);
  const idx = selected ? flatOrder.findIndex((d) => d.slug === selected.slug) : -1;
  const prev = idx > 0 ? flatOrder[idx - 1] : null;
  const next = idx >= 0 && idx < flatOrder.length - 1 ? flatOrder[idx + 1] : null;

  return (
    <div className="-m-6">  {/* break out of the dashboard layout's padding */}
      {/* ============= Top bar ============= */}
      <header className="sticky top-0 z-30 backdrop-blur bg-bg/80 border-b border-border">
        <div className="max-w-[1320px] mx-auto px-6 lg:px-10 h-14 flex items-center gap-3">
          <button
            onClick={() => setNavOpen(true)}
            className="lg:hidden p-1.5 rounded hover:bg-surface text-muted"
            aria-label="open nav"
          >
            <Menu size={18} />
          </button>
          <Link href="/dashboard/docs" onClick={() => setSelectedSlug(null)}
            className="flex items-center gap-2 text-sm font-medium">
            <BookOpen size={16} className="text-muted" /> Documentation
          </Link>

          <div className="ml-auto flex items-center gap-2">
            <div className="hidden md:flex items-center gap-2 w-72 px-3 py-1.5 bg-surface border border-border rounded-full text-sm">
              <Search size={13} className="text-muted shrink-0" />
              <input
                placeholder="Search docs"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                className="bg-transparent outline-none w-full placeholder:text-muted text-text"
              />
              <kbd className="hidden lg:inline text-[10px] text-muted border border-border rounded px-1 font-mono">/</kbd>
            </div>
            <ThemeToggle />
          </div>
        </div>
      </header>

      {/* ============= Hero (only on landing) ============= */}
      {!selected && <Hero source={resp?.source} bucket={resp?.bucket} onPick={setSelectedSlug} />}

      {/* ============= Main grid ============= */}
      <div className="max-w-[1320px] mx-auto px-6 lg:px-10 py-10">
        <div className={selected
          ? "lg:grid lg:grid-cols-[260px_minmax(0,1fr)_220px] lg:gap-12"
          : "lg:grid lg:grid-cols-[260px_minmax(0,1fr)] lg:gap-12"}>

          {/* --- Left nav (sticky) --- */}
          <aside className="hidden lg:block">
            <SideNav
              grouped={grouped}
              selectedSlug={selectedSlug}
              onPick={setSelectedSlug}
            />
          </aside>

          {/* --- Off-canvas nav (mobile) --- */}
          {navOpen && (
            <div className="lg:hidden fixed inset-0 z-40 flex">
              <div className="absolute inset-0 bg-black/30" onClick={() => setNavOpen(false)} />
              <div className="relative w-72 max-w-[80vw] bg-surface border-r border-border overflow-y-auto p-6">
                <button
                  onClick={() => setNavOpen(false)}
                  className="absolute top-3 right-3 p-1.5 rounded hover:bg-bg text-muted"
                  aria-label="close nav"
                >
                  <X size={16} />
                </button>
                <SideNav
                  grouped={grouped}
                  selectedSlug={selectedSlug}
                  onPick={(s) => { setSelectedSlug(s); setNavOpen(false); }}
                />
              </div>
            </div>
          )}

          {/* --- Center: reader --- */}
          <main className="min-w-0">
            {!selected && resp && <SectionsOverview grouped={grouped} onPick={setSelectedSlug} />}

            {selected && (
              <article>
                {/* Breadcrumb */}
                <nav className="text-[13px] text-muted flex items-center gap-2 mb-6">
                  <button onClick={() => setSelectedSlug(null)} className="hover:text-text">Docs</button>
                  {selected.section && (
                    <>
                      <span>/</span>
                      <span>{SECTION_LABEL[selected.section] || selected.section}</span>
                    </>
                  )}
                </nav>

                {loading && <ReaderSkeleton />}
                {!loading && (
                  <>
                    <div className="prose"
                      dangerouslySetInnerHTML={{ __html: parsed.html }} />

                    {/* Standalone-page + raw-markdown links */}
                    {(selected.pageUrl || selected.url) && (
                      <div className="mt-16 pt-6 border-t border-border-subtle text-xs text-muted flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2">
                        <span>
                          {selected.pageUrl && (
                            <>Permalink:{" "}
                              <a href={selected.pageUrl} target="_blank" rel="noreferrer"
                                className="text-link underline underline-offset-2 hover:no-underline break-all">
                                {selected.pageUrl}
                              </a>
                            </>
                          )}
                        </span>
                        <div className="flex gap-3 shrink-0">
                          {selected.pageUrl && (
                            <a href={selected.pageUrl} target="_blank" rel="noreferrer"
                              className="inline-flex items-center gap-1 hover:text-text">
                              Open page <ExternalLink size={11} />
                            </a>
                          )}
                          {selected.url && (
                            <a href={selected.url} target="_blank" rel="noreferrer"
                              className="inline-flex items-center gap-1 hover:text-text">
                              Raw .md <ExternalLink size={11} />
                            </a>
                          )}
                        </div>
                      </div>
                    )}

                    {/* Prev / next nav */}
                    {(prev || next) && (
                      <div className="mt-12 grid grid-cols-2 gap-4">
                        <PrevNext doc={prev} dir="prev" onClick={setSelectedSlug} />
                        <PrevNext doc={next} dir="next" onClick={setSelectedSlug} />
                      </div>
                    )}
                  </>
                )}
              </article>
            )}
          </main>

          {/* --- Right rail: ToC --- */}
          {selected && parsed.toc.length > 0 && (
            <aside className="hidden lg:block">
              <TableOfContents toc={parsed.toc} />
            </aside>
          )}
        </div>
      </div>
    </div>
  );
}

// ============================================================================
// Hero — only on the landing page (no doc selected)
// ============================================================================

function Hero({ source, bucket, onPick }: {
  source?: "bucket" | "disk";
  bucket?: string;
  onPick: (slug: string) => void;
}) {
  return (
    <section className="border-b border-border">
      <div className="max-w-[1320px] mx-auto px-6 lg:px-10 py-20 lg:py-28">
        <div className="max-w-2xl">
          <p className="text-sm text-muted uppercase tracking-[0.15em] mb-4">Documentation</p>
          <h1 className="text-4xl lg:text-5xl font-semibold tracking-tight leading-[1.1]">
            Everything you can do with PersonalS3 —
            <span className="text-muted"> three ways for each task.</span>
          </h1>
          <p className="mt-6 text-lg text-text-soft leading-relaxed max-w-xl">
            One self-hosted, S3-compatible store. Browse from the
            dashboard, script with the <code className="font-mono text-[0.92em] bg-codeBg px-1 py-0.5 rounded">ps3</code> CLI,
            or call the HTTP API directly — pick whichever fits the moment.
          </p>
          <div className="mt-10 flex flex-wrap gap-3">
            <button
              onClick={() => onPick("getting-started")}
              className="px-5 py-2.5 bg-text text-bg text-sm font-medium rounded-full hover:opacity-90 transition-opacity"
            >
              Start with the basics →
            </button>
            <button
              onClick={() => onPick("api-reference")}
              className="px-5 py-2.5 border border-border text-sm font-medium rounded-full hover:border-text/40 transition-colors"
            >
              API reference
            </button>
          </div>
          {source === "bucket" && bucket && (
            <p className="mt-8 text-xs text-muted">
              These docs are served straight from a public PersonalS3 bucket called{" "}
              <span className="font-mono text-text-soft">{bucket}</span>. We dogfood our own product.
            </p>
          )}
        </div>
      </div>
    </section>
  );
}

// ============================================================================
// Sections overview — the landing "what's in here" cards
// ============================================================================

const SECTION_BADGE: Record<string, { icon: React.ReactNode; copy: string }> = {
  "":              { icon: <BookOpen    size={18} />, copy: "Your first five minutes." },
  account:         { icon: <KeyRound    size={18} />, copy: "Sign in, install the CLI, manage keys." },
  uploading:       { icon: <Upload      size={18} />, copy: "Put files into a bucket — three ways for each." },
  files:           { icon: <FolderTree  size={18} />, copy: "List, share, version, recover from trash." },
  streaming:       { icon: <PlayCircle  size={18} />, copy: "Watch your videos, play your audio." },
  quotas:          { icon: <Wallet      size={18} />, copy: "What counts, what to do when you're full." },
  troubleshooting: { icon: <LifeBuoy    size={18} />, copy: "When something doesn't behave." },
};

function SectionsOverview({ grouped, onPick }: {
  grouped: Record<string, DocEntry[]>;
  onPick: (slug: string) => void;
}) {
  return (
    <div className="space-y-16">
      {SECTION_ORDER.filter((s) => (grouped[s]?.length ?? 0) > 0).map((section) => {
        const meta = SECTION_BADGE[section] || { icon: <BookOpen size={18} />, copy: "" };
        return (
          <section key={section}>
            <div className="flex items-baseline gap-3 mb-6">
              <span className="text-muted">{meta.icon}</span>
              <h2 className="text-2xl font-semibold tracking-tight">
                {SECTION_LABEL[section] || section}
              </h2>
              {meta.copy && <span className="text-sm text-muted">— {meta.copy}</span>}
            </div>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              {grouped[section].map((d) => (
                <button
                  key={d.slug}
                  onClick={() => onPick(d.slug)}
                  className="text-left p-6 bg-surface border border-border rounded-lg hover:border-text/30 transition-colors group"
                >
                  <h3 className="text-base font-semibold mb-2 group-hover:text-link transition-colors">{d.title}</h3>
                  <p className="text-sm text-text-soft leading-relaxed line-clamp-2">{d.summary}</p>
                </button>
              ))}
            </div>
          </section>
        );
      })}
    </div>
  );
}

// ============================================================================
// Side nav
// ============================================================================

function SideNav({ grouped, selectedSlug, onPick }: {
  grouped: Record<string, DocEntry[]>;
  selectedSlug: string | null;
  onPick: (slug: string) => void;
}) {
  return (
    <nav className="sticky top-24 max-h-[calc(100vh-7rem)] overflow-y-auto pr-2">
      {SECTION_ORDER.filter((s) => (grouped[s]?.length ?? 0) > 0).map((section) => (
        <div key={section} className="mb-7">
          <h4 className="text-[11px] uppercase tracking-[0.12em] text-muted font-semibold mb-2.5 px-2">
            {SECTION_LABEL[section] || section}
          </h4>
          <ul className="space-y-px">
            {grouped[section].map((d) => (
              <li key={d.slug}>
                <button
                  onClick={() => onPick(d.slug)}
                  className={
                    "block w-full text-left px-2 py-1.5 rounded text-[13.5px] leading-snug transition-colors " +
                    (selectedSlug === d.slug
                      ? "text-text font-medium bg-surface"
                      : "text-text-soft hover:text-text hover:bg-surface/60")
                  }
                  title={d.summary}
                >
                  {d.title}
                </button>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  );
}

// ============================================================================
// Right rail — Table of Contents
// ============================================================================

function TableOfContents({ toc }: { toc: TocEntry[] }) {
  const [active, setActive] = useState<string | null>(null);

  useEffect(() => {
    const obs = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top)[0];
        if (visible) setActive(visible.target.id);
      },
      { rootMargin: "-20% 0px -70% 0px", threshold: 0 },
    );
    toc.forEach((t) => {
      const el = document.getElementById(t.id);
      if (el) obs.observe(el);
    });
    return () => obs.disconnect();
  }, [toc]);

  return (
    <nav className="sticky top-24 max-h-[calc(100vh-7rem)] overflow-y-auto">
      <p className="text-[11px] uppercase tracking-[0.12em] text-muted font-semibold mb-3">On this page</p>
      <ul className="space-y-1.5 text-[13px]">
        {toc.map((t) => (
          <li key={t.id} style={{ paddingLeft: `${(t.level - 2) * 12}px` }}>
            <a
              href={`#${t.id}`}
              className={
                "block py-0.5 leading-snug transition-colors " +
                (active === t.id
                  ? "text-link font-medium"
                  : "text-text-soft hover:text-text")
              }
            >
              {t.text}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

// ============================================================================
// Prev / next at the bottom of an article
// ============================================================================

function PrevNext({ doc, dir, onClick }: {
  doc: DocEntry | null;
  dir: "prev" | "next";
  onClick: (slug: string) => void;
}) {
  if (!doc) return <div />;
  return (
    <button
      onClick={() => onClick(doc.slug)}
      className={
        "flex flex-col gap-1 p-5 border border-border rounded-lg hover:border-text/30 transition-colors group " +
        (dir === "next" ? "text-right items-end" : "")
      }
    >
      <span className="text-[11px] uppercase tracking-[0.12em] text-muted flex items-center gap-1">
        {dir === "prev" && <ArrowLeft size={11} />}
        {dir}
        {dir === "next" && <ArrowRight size={11} />}
      </span>
      <span className="text-sm font-medium group-hover:text-link transition-colors">{doc.title}</span>
    </button>
  );
}

// ============================================================================

function ReaderSkeleton() {
  return (
    <div className="space-y-4 animate-pulse">
      <div className="h-12 bg-surface rounded w-3/4" />
      <div className="h-5 bg-surface rounded w-full" />
      <div className="h-5 bg-surface rounded w-5/6" />
      <div className="h-5 bg-surface rounded w-2/3" />
      <div className="h-32 bg-surface rounded mt-8" />
    </div>
  );
}
