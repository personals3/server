"use client";

// Public landing page.
//
// Signed-in users get bounced straight to the dashboard. Strangers see
// a real introduction — hero, three CTAs, a glance at what's inside,
// and links to the docs. Single-column, generous whitespace, the same
// warm-stone palette + Inter typography as the rest of the app.

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { getToken } from "@/lib/api";
import { ThemeToggle } from "@/components/theme-toggle";
import {
  Database, BookOpen, KeyRound, ArrowRight, Upload, Share2,
  PlayCircle, Wallet, Terminal,
} from "lucide-react";

export default function HomePage() {
  const router = useRouter();
  const [ready, setReady] = useState(false);

  // Signed-in users skip the landing page entirely.
  useEffect(() => {
    if (getToken()) {
      router.replace("/dashboard");
      return;
    }
    setReady(true);
  }, [router]);

  if (!ready) return <div className="min-h-screen bg-bg" />;

  return (
    <div className="min-h-screen bg-bg text-text">
      {/* ---- Top bar ---- */}
      <header className="sticky top-0 z-30 backdrop-blur bg-bg/85 border-b border-border-subtle">
        <div className="max-w-6xl mx-auto h-16 px-6 lg:px-10 flex items-center gap-6">
          <Link href="/" className="flex items-center gap-2 text-sm font-semibold tracking-tight">
            <Database size={16} className="text-link" /> PersonalS3
          </Link>
          <nav className="hidden sm:flex items-center gap-5 text-sm text-text-soft">
            <Link href="/docs" className="hover:text-text">Docs</Link>
            <Link href="/login#request" className="hover:text-text">Request access</Link>
          </nav>
          <div className="ml-auto flex items-center gap-3">
            <ThemeToggle />
            <Link href="/login"
              className="text-sm font-medium px-3.5 py-1.5 rounded-md bg-text text-bg hover:opacity-90 transition-opacity inline-flex items-center gap-1">
              Sign in <ArrowRight size={13} />
            </Link>
          </div>
        </div>
      </header>

      {/* ---- Hero ---- */}
      <section className="border-b border-border-subtle">
        <div className="max-w-5xl mx-auto px-6 lg:px-10 py-20 lg:py-32">
          <p className="text-[11px] uppercase tracking-[0.2em] text-muted mb-5">
            Self-hosted · S3-compatible
          </p>
          <h1 className="text-4xl sm:text-5xl lg:text-6xl font-semibold tracking-[-0.035em] leading-[1.05] max-w-3xl">
            Your own storage cloud.
            <br />
            <span className="text-muted">Quiet, owned, accountable.</span>
          </h1>
          <p className="mt-8 text-lg text-text-soft leading-relaxed max-w-2xl">
            PersonalS3 gives you a private place for your files, videos, and backups
            — accessed from the dashboard, the <code className="font-mono text-[0.92em] bg-codeBg px-1.5 py-0.5 rounded border border-border">ps3</code> CLI,
            or any S3-compatible client. No vendor lock-in. No surprise bills.
          </p>
          <div className="mt-10 flex flex-wrap gap-3">
            <Link href="/login"
              className="px-5 py-2.5 bg-text text-bg text-sm font-medium rounded-full hover:opacity-90 inline-flex items-center gap-1.5">
              <KeyRound size={14} /> Sign in
            </Link>
            <Link href="/login#request"
              className="px-5 py-2.5 border border-border text-sm font-medium rounded-full hover:border-text/40">
              Request access
            </Link>
            <Link href="/docs"
              className="px-5 py-2.5 text-text-soft text-sm font-medium rounded-full hover:text-text inline-flex items-center gap-1.5">
              <BookOpen size={14} /> Read the docs
            </Link>
          </div>
        </div>
      </section>

      {/* ---- Three things that matter ---- */}
      <section className="max-w-5xl mx-auto px-6 lg:px-10 py-20 lg:py-28">
        <p className="text-[11px] uppercase tracking-[0.2em] text-muted mb-5">What you get</p>
        <div className="grid grid-cols-1 md:grid-cols-3 gap-8">
          <Pillar icon={<Upload size={20} />} title="Upload anything">
            Drag-drop in the dashboard, <code className="font-mono text-[0.9em]">ps3 cp</code> from
            the terminal, or call the HTTP API directly. Multipart for big files
            is automatic.
          </Pillar>
          <Pillar icon={<PlayCircle size={20} />} title="Stream your media">
            Videos auto-transcode to HLS with multiple qualities. Audio gets MP3/OGG
            fallbacks. Embed in your own pages or use the built-in player.
          </Pillar>
          <Pillar icon={<Share2 size={20} />} title="Share on your terms">
            Time-limited signed URLs, public buckets, or fully-private. Revoke
            access instantly. No third party in the loop.
          </Pillar>
        </div>
      </section>

      {/* ---- Three ways to work ---- */}
      <section className="border-t border-border-subtle bg-surface/40">
        <div className="max-w-5xl mx-auto px-6 lg:px-10 py-20 lg:py-28">
          <p className="text-[11px] uppercase tracking-[0.2em] text-muted mb-5">Three ways in</p>
          <h2 className="text-3xl sm:text-4xl font-semibold tracking-tight max-w-2xl">
            One service, three interfaces.
            <span className="text-muted"> Pick whichever fits the moment.</span>
          </h2>

          <div className="mt-12 grid grid-cols-1 md:grid-cols-3 gap-6">
            <Lane title="Dashboard" tag="for humans"
              copy="Drag-drop uploads, file previews, sharing, search, trash. Works on phone and desktop."
              ctaHref="/login" ctaLabel="Sign in" />
            <Lane title="ps3 CLI" tag="for scripts"
              copy="Single binary. Cross-platform. Pipe-friendly output. Resumable uploads for big files."
              ctaHref="/docs/account/installing-the-cli.html" ctaLabel="Install guide" />
            <Lane title="HTTP API" tag="for integrations"
              copy="S3-compatible signing. JSON or XML responses. Field-by-field reference for every endpoint."
              ctaHref="/docs/uploading/single-put-api.html" ctaLabel="API reference" />
          </div>

          <div className="mt-12 bg-codeBg border border-border rounded-lg p-4 sm:p-5 max-w-2xl font-mono text-sm overflow-x-auto">
            <p className="text-muted text-[11px] mb-2">Example: upload a file</p>
            <pre className="text-text whitespace-pre-wrap"># Dashboard: drag onto the bucket page

# CLI
ps3 cp ./photo.jpg my-bucket/photos/photo.jpg

# HTTP
curl -X PUT https://your-instance.example/api/my-bucket/photo.jpg \
  -H {'"'}Authorization: Bearer $TOKEN{'"'} \
  --data-binary @./photo.jpg</pre>
          </div>
        </div>
      </section>

      {/* ---- CTA strip ---- */}
      <section className="border-t border-border-subtle">
        <div className="max-w-5xl mx-auto px-6 lg:px-10 py-20 lg:py-24 text-center">
          <h2 className="text-3xl sm:text-4xl font-semibold tracking-tight">
            Ready when you are.
          </h2>
          <p className="mt-4 text-text-soft max-w-xl mx-auto">
            New accounts start with 100 MB free. Ask the administrator for more once you&apos;re in.
          </p>
          <div className="mt-8 flex flex-wrap justify-center gap-3">
            <Link href="/login#request"
              className="px-5 py-2.5 bg-text text-bg text-sm font-medium rounded-full hover:opacity-90 inline-flex items-center gap-1.5">
              Request access <ArrowRight size={13} />
            </Link>
            <Link href="/docs"
              className="px-5 py-2.5 border border-border text-sm font-medium rounded-full hover:border-text/40">
              Read the docs
            </Link>
          </div>
        </div>
      </section>

      {/* ---- Footer ---- */}
      <footer className="border-t border-border-subtle">
        <div className="max-w-5xl mx-auto px-6 lg:px-10 py-8 flex flex-wrap items-center justify-between gap-4 text-xs text-muted">
          <span>© PersonalS3. Operated by this instance&apos;s administrator.</span>
          <nav className="flex gap-4">
            <Link href="/docs" className="hover:text-text inline-flex items-center gap-1">
              <BookOpen size={11} /> Docs
            </Link>
            <Link href="/login" className="hover:text-text inline-flex items-center gap-1">
              <KeyRound size={11} /> Sign in
            </Link>
          </nav>
        </div>
      </footer>
    </div>
  );
}

// ----------------------------------------------------------------------------

function Pillar({ icon, title, children }: {
  icon: React.ReactNode; title: string; children: React.ReactNode;
}) {
  return (
    <div>
      <div className="inline-flex items-center justify-center w-10 h-10 rounded-md border border-border text-text-soft mb-4">
        {icon}
      </div>
      <h3 className="text-lg font-semibold tracking-tight">{title}</h3>
      <p className="mt-2 text-text-soft leading-relaxed">{children}</p>
    </div>
  );
}

function Lane({ title, tag, copy, ctaHref, ctaLabel }: {
  title: string; tag: string; copy: string;
  ctaHref: string; ctaLabel: string;
}) {
  return (
    <div className="bg-surface border border-border rounded-xl p-6 flex flex-col gap-4 hover:border-text/30 transition-colors">
      <div className="flex items-baseline justify-between">
        <h3 className="text-lg font-semibold tracking-tight">{title}</h3>
        <span className="text-[10px] uppercase tracking-[0.12em] text-muted">{tag}</span>
      </div>
      <p className="text-text-soft text-sm leading-relaxed flex-1">{copy}</p>
      <Link href={ctaHref}
        className="text-sm font-medium text-text hover:text-link inline-flex items-center gap-1">
        {ctaLabel} <ArrowRight size={12} />
      </Link>
    </div>
  );
}
