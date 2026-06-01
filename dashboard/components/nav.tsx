"use client";

// Side nav — responsive shell.
//
//  >=lg: persistent left rail (260px), top header is hidden
//   <lg: top bar with hamburger; clicking it slides a drawer in from
//        the left with a backdrop. Drawer closes on link click, on
//        backdrop click, on Escape, and on route change.
//
// The drawer is keyed off `usePathname` so back/forward in the browser
// auto-dismisses it without any extra wiring.

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { setToken } from "@/lib/api";
import { userStore, User } from "@/lib/user";
import {
  Database, Key, LayoutDashboard, LogOut, Users, FileSearch, Activity,
  Trash2, Sparkles, Search, Link2, Shield, ScrollText, BookOpen,
  Inbox, Menu, X, ExternalLink, HardDrive,
} from "lucide-react";
import { ThemeToggle } from "@/components/theme-toggle";
import { cn, formatBytes } from "@/lib/format";

const userItems = [
  { href: "/dashboard",          label: "Overview",     icon: LayoutDashboard },
  { href: "/dashboard/buckets",  label: "Buckets",      icon: Database },
  { href: "/dashboard/shares",   label: "Share links",  icon: Link2 },
  { href: "/dashboard/keys",     label: "API Keys",     icon: Key },
  { href: "/dashboard/security", label: "Security",     icon: Shield },
  { href: "/dashboard/trash",    label: "Trash",        icon: Trash2 },
];

// External docs site — opens in a new tab, rendered separately below
// the main nav items so users can tell it leaves the app.
const DOCS_URL = "https://developers.personals3.tech";

const adminItems = [
  { href: "/dashboard/admin/users",    label: "Users",     icon: Users },
  { href: "/dashboard/admin/requests", label: "Requests",  icon: Inbox },
  { href: "/dashboard/admin/audit",    label: "Audit Log", icon: FileSearch },
  { href: "/dashboard/admin/logs",     label: "Logs",      icon: ScrollText },
  { href: "/dashboard/admin/system",   label: "System",    icon: Activity },
  { href: "/dashboard/admin/cleanup",  label: "Cleanup",   icon: Sparkles },
];

export function Nav() {
  const path = usePathname();
  const router = useRouter();
  const [user, setUser] = useState<User | null>(userStore.get());
  const [drawerOpen, setDrawerOpen] = useState(false);

  useEffect(() => userStore.subscribe(setUser), []);

  // Auto-close drawer when route changes.
  useEffect(() => { setDrawerOpen(false); }, [path]);

  // Escape closes drawer.
  useEffect(() => {
    if (!drawerOpen) return;
    const h = (e: KeyboardEvent) => { if (e.key === "Escape") setDrawerOpen(false); };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [drawerOpen]);

  // Cmd/Ctrl+K → jump to search.
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        router.push("/dashboard/search");
      }
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [router]);

  const onSearchPage = path === "/dashboard/search" || path.startsWith("/dashboard/search/");
  const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

  const body = (
    <>
      {/* Brand */}
      <Link href="/dashboard" className="flex items-center gap-2 mb-5 group">
        <img src="/icon.svg" alt="" width={22} height={22} className="rounded" />
        <span className="text-[15px] font-semibold tracking-tight group-hover:text-link transition-colors">
          PersonalS3
        </span>
      </Link>

      {/* Quick search trigger — looks like an input, behaves like a button. */}
      <button
        type="button"
        onClick={() => router.push("/dashboard/search")}
        className={cn(
          "flex items-center gap-2 w-full px-2.5 py-1.5 mb-5 rounded-md text-left",
          "border border-border-subtle bg-bg/40 hover:bg-surface/60 hover:border-border",
          "transition-colors group",
          onSearchPage && "border-border bg-surface/60",
        )}
      >
        <Search size={14} className="text-muted shrink-0" />
        <span className="text-xs text-muted flex-1 truncate group-hover:text-text-soft transition-colors">
          Search files...
        </span>
        <kbd className="hidden xl:inline-flex items-center gap-0.5 text-[10px] text-muted font-mono px-1 py-px rounded border border-border-subtle bg-bg/60 shrink-0">
          {isMac ? "⌘" : "Ctrl"}K
        </kbd>
      </button>

      {/* Primary nav */}
      <nav className="flex-1 overflow-y-auto -mx-1 px-1 space-y-5">
        <Section label="Workspace">
          {userItems.map((item) => (
            <NavItem key={item.href} {...item} active={isActive(path, item.href)} />
          ))}
        </Section>

        {user?.role === "admin" && (
          <Section label="Admin">
            {adminItems.map((item) => (
              <NavItem key={item.href} {...item} active={isActive(path, item.href)} />
            ))}
          </Section>
        )}
      </nav>

      {/* Storage indicator — ambient visibility of fill level. */}
      {user && <StorageMini used={user.usedBytes} quota={user.quotaBytes} />}

      {/* User card */}
      {user && <UserCard user={user} />}

      {/* Footer actions — heights aligned to ThemeToggle's h-9 rounded pill. */}
      <div className="mt-2 flex items-center gap-1.5">
        <a
          href={DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="flex-1 h-9 inline-flex items-center justify-center gap-1.5 px-2 rounded-full text-xs text-text-soft border border-border hover:bg-surface hover:text-text hover:border-text/40 transition-colors"
          title="Open developer docs"
        >
          <BookOpen size={13} />
          Docs
          <ExternalLink size={10} className="text-muted" />
        </a>
        <ThemeToggle />
        <button
          onClick={() => { setToken(null); userStore.set(null); router.push("/login"); }}
          className="w-9 h-9 inline-flex items-center justify-center rounded-full border border-border text-muted hover:bg-surface hover:text-danger hover:border-danger/40 transition-colors"
          title="Log out"
          aria-label="Log out"
        >
          <LogOut size={14} />
        </button>
      </div>
    </>
  );

  return (
    <>
      {/* Mobile top bar */}
      <header className="lg:hidden sticky top-0 z-30 h-14 flex items-center px-4 border-b border-border bg-bg/95 backdrop-blur">
        <button onClick={() => setDrawerOpen(true)}
          className="p-1.5 -ml-1.5 rounded-md hover:bg-surface text-text" aria-label="open menu">
          <Menu size={20} />
        </button>
        <Link href="/dashboard" className="ml-3 inline-flex items-center gap-1.5 text-sm font-semibold">
          <img src="/icon.svg" alt="" width={18} height={18} className="rounded" />
          PersonalS3
        </Link>
        <div className="ml-auto"><ThemeToggle /></div>
      </header>

      {/* Persistent sidebar on lg+ — sticky h-screen so it has its own
          scrollbar and viewport height; content height is independent. */}
      <aside className="hidden lg:flex sticky top-0 h-screen w-[260px] shrink-0 border-r border-border bg-panel p-4 flex-col overflow-y-auto">
        {body}
      </aside>

      {/* Off-canvas drawer on <lg */}
      {drawerOpen && (
        <div className="lg:hidden fixed inset-0 z-50">
          <div className="absolute inset-0 bg-black/40 backdrop-blur-sm" onClick={() => setDrawerOpen(false)} />
          <aside className="absolute left-0 top-0 bottom-0 w-72 max-w-[85vw] bg-panel border-r border-border p-4 flex flex-col shadow-2xl animate-in slide-in-from-left overflow-y-auto">
            <button onClick={() => setDrawerOpen(false)}
              className="absolute top-3 right-3 p-1.5 rounded-md hover:bg-surface text-muted" aria-label="close menu">
              <X size={16} />
            </button>
            {body}
          </aside>
        </div>
      )}
    </>
  );
}

function isActive(path: string, href: string): boolean {
  if (href === "/dashboard") return path === "/dashboard";
  return path === href || path.startsWith(href + "/");
}

function Section({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <p className="text-[10px] uppercase tracking-[0.12em] text-muted px-2.5 mb-1.5 font-medium">
        {label}
      </p>
      <div className="space-y-px">{children}</div>
    </div>
  );
}

function NavItem({ href, label, icon: Icon, active }: {
  href: string;
  label: string;
  icon: typeof Database;
  active: boolean;
}) {
  return (
    <Link
      href={href}
      className={cn(
        "relative flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-[13px] transition-colors",
        active
          ? "bg-surface text-text font-medium"
          : "text-text-soft hover:bg-surface/60 hover:text-text",
      )}
    >
      {/* Left-edge accent strip on active item — much more findable than
          subtle background alone. */}
      {active && (
        <span className="absolute left-0 top-1.5 bottom-1.5 w-[2px] rounded-r bg-text" />
      )}
      <Icon size={15} className={active ? "text-text" : "text-muted"} />
      {label}
    </Link>
  );
}

function StorageMini({ used, quota }: { used: number; quota: number }) {
  const pct = quota > 0 ? Math.min(100, (used / quota) * 100) : 0;
  const near = pct >= 90;
  const warn = pct >= 75 && !near;
  return (
    <Link
      href="/dashboard"
      className="mt-4 mb-3 mx-0.5 block group"
      title={`${formatBytes(used)} of ${formatBytes(quota)} used`}
    >
      <div className="flex items-center justify-between text-[10px] uppercase tracking-[0.1em] mb-1.5 px-1">
        <span className="text-muted inline-flex items-center gap-1">
          <HardDrive size={10} /> Storage
        </span>
        <span className={cn(
          "tabular-nums font-mono",
          near ? "text-danger" : warn ? "text-warning" : "text-text-soft",
        )}>
          {pct.toFixed(0)}%
        </span>
      </div>
      <div className="h-1 rounded-full bg-surface overflow-hidden">
        <div
          className={cn(
            "h-full transition-all duration-500",
            near ? "bg-danger" : warn ? "bg-warning" : "bg-text/80",
          )}
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="mt-1 text-[10px] text-muted font-mono px-1 group-hover:text-text-soft transition-colors">
        {formatBytes(used)} / {formatBytes(quota)}
      </div>
    </Link>
  );
}

function UserCard({ user }: { user: User }) {
  const initials = (user.name || user.email)
    .split(/[\s@.]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((s) => s[0]?.toUpperCase())
    .join("") || "?";
  const displayName = user.name?.trim() || user.email.split("@")[0];
  return (
    <div className="flex items-center gap-2 px-1 py-2 border-t border-border-subtle">
      <div className="w-7 h-7 rounded-full bg-surface border border-border-subtle flex items-center justify-center text-[10px] font-semibold tracking-wide text-text-soft shrink-0">
        {initials}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5">
          <p className="text-xs font-medium truncate" title={displayName}>
            {displayName}
          </p>
          {user.role === "admin" && (
            <span className="text-[9px] uppercase tracking-wider bg-codeBg text-text-soft px-1 py-px rounded border border-border-subtle shrink-0">
              admin
            </span>
          )}
        </div>
        <p className="text-[10px] text-muted truncate font-mono" title={user.email}>
          {user.email}
        </p>
      </div>
    </div>
  );
}
