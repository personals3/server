import "./globals.css";
import type { Metadata } from "next";
import { Inter, JetBrains_Mono } from "next/font/google";

// Inter for everything — luxury docs sites (Stripe, Linear, Vercel) all
// converge on it. JetBrains Mono for code: wider character set, ligatures
// off feels more "neutral" and reads better at small sizes than Fira Code.
const sans = Inter({
  subsets: ["latin"],
  display: "swap",
  variable: "--font-sans",
});
const mono = JetBrains_Mono({
  subsets: ["latin"],
  display: "swap",
  variable: "--font-mono",
});

export const metadata: Metadata = {
  title: "PersonalS3 Dashboard",
  description: "Self-hosted S3 with media transcoding",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning className={`${sans.variable} ${mono.variable}`}>
      <head>
        {/* Apply saved theme BEFORE first paint so we don't flash the wrong
            background. Reads from localStorage, falls back to OS pref. */}
        <script
          dangerouslySetInnerHTML={{
            __html: `(function(){try{
              var t = localStorage.getItem('ps3_theme');
              if (!t) t = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
              document.documentElement.setAttribute('data-theme', t);
            }catch(e){}})();`,
          }}
        />
      </head>
      <body className="min-h-screen bg-bg text-text antialiased font-sans">{children}</body>
    </html>
  );
}
