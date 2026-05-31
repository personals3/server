// Layout for the public file-explorer at /p/{bucket}/...
//
// Deliberately minimal: no sidebar, no auth check, no userStore. The whole
// point of /p/ is that anyone with the URL can land here without an account.
// We keep the warm-stone palette + theme toggle so the visual language stays
// consistent with the rest of the app.

import Link from "next/link";
import { ThemeToggle } from "@/components/theme-toggle";
import { Globe } from "lucide-react";

export default function PublicExplorerLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen flex flex-col">
      {/* Top bar — brand on the left, theme toggle on the right. */}
      <header className="sticky top-0 z-30 backdrop-blur bg-bg/80 border-b border-border">
        <div className="max-w-[1100px] mx-auto px-4 sm:px-6 lg:px-10 h-14 flex items-center gap-3">
          <Link href="/" className="flex items-center gap-2 text-sm font-medium hover:text-link transition-colors">
            <Globe size={16} className="text-muted" />
            <span>PersonalS3</span>
            <span className="text-muted">·</span>
            <span className="text-muted">Public</span>
          </Link>
          <div className="ml-auto flex items-center gap-2">
            <ThemeToggle />
          </div>
        </div>
      </header>
      <main className="flex-1">{children}</main>
    </div>
  );
}
