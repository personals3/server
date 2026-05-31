"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { getToken, api } from "@/lib/api";
import { Nav } from "@/components/nav";
import { ToastHost } from "@/components/toast";
import { userStore, User } from "@/lib/user";

export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const [ready, setReady] = useState(false);

  useEffect(() => {
    if (!getToken()) { router.replace("/login"); return; }
    api<User>("/auth/me")
      .then((u) => { userStore.set(u); setReady(true); })
      .catch(() => { router.replace("/login"); });

    // Multi-tab auth sync — if another tab signs out (clears the token)
    // or signs in as a different user (token changes), reload so we
    // pick up the right session instead of firing stale requests.
    const onStorage = (e: StorageEvent) => {
      if (e.key !== "ps3_token") return;
      if (!e.newValue) {
        // Sign-out from another tab → bounce to login
        router.replace("/login");
      } else if (e.oldValue && e.oldValue !== e.newValue) {
        // Different user signed in elsewhere → hard reload so all state
        // (user, buckets, polls) is reset under the new identity
        window.location.reload();
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [router]);

  if (!ready) {
    return (
      <main className="min-h-screen flex items-center justify-center text-muted">
        Loading...
      </main>
    );
  }

  return (
    <ToastHost>
      <div className="lg:flex min-h-screen">
        <Nav />
        {/* min-w-0 lets long content (file keys, tables) overflow + scroll
            instead of pushing the sidebar off-screen. */}
        <main className="flex-1 min-w-0 p-4 sm:p-6 lg:p-8">
          <div className="mx-auto w-full max-w-screen-2xl">{children}</div>
        </main>
      </div>
    </ToastHost>
  );
}
