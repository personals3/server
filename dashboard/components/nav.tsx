"use client";

// Side nav — responsive shell.
//
//  >=lg: persistent left rail (256px), top header is hidden
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
  Inbox, Menu, X, ExternalLink,
} from "lucide-react";
import { ThemeToggle } from "@/components/theme-toggle";
import { cn } from "@/lib/format";

const userItems = [
  { href: "/dashboard",          label: "Overview",     icon: LayoutDashboard },
  { href: "/dashboard/buckets",  label: "Buckets",      icon: Database },
  { href: "/dashboard/search",   label: "Search",       icon: Search },
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

  const body = (
    <>
      <div className="mb-6">
        <h1 className="text-lg font-semibold tracking-tight">PersonalS3</h1>
        <p className="text-xs text-muted mt-0.5">self-hosted storage</p>
        {user && (
          <div className="mt-4 pt-4 border-t border-border-subtle">
            <p className="text-xs font-mono text-text-soft truncate" title={user.email}>
              {user.email}
            </p>
            {user.role === "admin" && (
              <span className="inline-block mt-1.5 text-[10px] uppercase tracking-wider bg-codeBg text-text-soft px-1.5 py-0.5 rounded border border-border">
                administrator
              </span>
            )}
          </div>
        )}
      </div>

      <nav className="flex-1 space-y-6 overflow-y-auto -mx-1 px-1">
        <div className="space-y-px">
          {userItems.map((item) => (
            <NavItem key={item.href} {...item} active={isActive(path, item.href)} />
          ))}
        </div>

        {user?.role === "admin" && (
          <div className="space-y-px">
            <p className="text-[10px] uppercase tracking-[0.15em] text-muted px-3 mb-1.5">
              Admin
            </p>
            {adminItems.map((item) => (
              <NavItem key={item.href} {...item} active={isActive(path, item.href)} />
            ))}
          </div>
        )}
      </nav>

      <div className="mt-6 space-y-px">
        <a
          href={DOCS_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="flex items-center gap-2.5 px-3 py-2 rounded-md text-[13.5px] text-text-soft hover:bg-surface/60 hover:text-text transition-colors"
        >
          <BookOpen size={15} className="text-muted" />
          <span>Docs</span>
          <ExternalLink size={11} className="ml-auto text-muted" />
        </a>
        <div className="flex items-center gap-2 pt-1">
          <button
            onClick={() => { setToken(null); userStore.set(null); router.push("/login"); }}
            className="flex-1 flex items-center gap-2 px-3 py-2 rounded-md text-sm text-text-soft hover:bg-surface hover:text-text transition-colors"
          >
            <LogOut size={15} /> Log out
          </button>
          <ThemeToggle />
        </div>
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
        <Link href="/dashboard" className="ml-3 text-sm font-semibold">PersonalS3</Link>
        <div className="ml-auto"><ThemeToggle /></div>
      </header>

      {/* Persistent sidebar on lg+ */}
      <aside className="hidden lg:flex w-64 shrink-0 border-r border-border bg-panel min-h-screen p-5 flex-col">
        {body}
      </aside>

      {/* Off-canvas drawer on <lg */}
      {drawerOpen && (
        <div className="lg:hidden fixed inset-0 z-50">
          <div className="absolute inset-0 bg-black/40 backdrop-blur-sm" onClick={() => setDrawerOpen(false)} />
          <aside className="absolute left-0 top-0 bottom-0 w-72 max-w-[85vw] bg-panel border-r border-border p-5 flex flex-col shadow-2xl animate-in slide-in-from-left">
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
        "flex items-center gap-2.5 px-3 py-2 rounded-md text-[13.5px] transition-colors",
        active
          ? "bg-surface text-text font-medium"
          : "text-text-soft hover:bg-surface/60 hover:text-text",
      )}
    >
      <Icon size={15} className={active ? "text-text" : "text-muted"} />
      {label}
    </Link>
  );
}
