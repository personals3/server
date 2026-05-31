import type { Config } from "tailwindcss";

// Tokens come from CSS variables in globals.css so a single
// data-theme="light|dark" flip retones everything — no class-based dark:
// prefixes needed and no flicker on first paint.
//
// Notes for using these:
//   - `bg`/`text`/`muted`/`border` are the four colours every page should
//     reach for first. Reaching for raw stone-* utility classes is a smell.
//   - `surface` vs `panel`: surface is the "page" background tone (so cards
//     pop from the bg), panel is the card itself. In dark mode they're the
//     same value — distinction matters mostly in light mode.
//   - `accent` is the action / focus colour (blue link or near-white).
//     Never use it as a brand fill — that's `text` (which is near-black
//     in light, near-white in dark, giving high-contrast monochrome CTAs
//     in the style of Linear / Vercel).
export default {
  content: [
    "./app/**/*.{ts,tsx}",
    "./components/**/*.{ts,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        bg:             "var(--color-bg)",
        panel:          "var(--color-panel)",
        surface:        "var(--color-surface)",
        border:         "var(--color-border)",
        "border-subtle": "var(--color-border-subtle)",
        muted:          "var(--color-muted)",
        text:           "var(--color-text)",
        "text-soft":    "var(--color-text-soft)",
        accent:         "var(--color-link)",   // historical alias — old code uses "accent"
        link:           "var(--color-link)",
        success:        "var(--color-success)",
        warning:        "var(--color-warning)",
        danger:         "var(--color-danger)",
        codeBg:         "var(--color-codeBg)",
      },
      fontFamily: {
        sans: ["var(--font-sans)", "Inter", "system-ui", "sans-serif"],
        mono: ["var(--font-mono)", "ui-monospace", "JetBrains Mono", "monospace"],
      },
      // Display sizes for hero/page headers. Tailwind's defaults stop at
      // 4.5rem which feels small for first-screen impact; doubled the top.
      fontSize: {
        "display-sm": ["2rem",  { lineHeight: "1.15", letterSpacing: "-0.02em" }],
        "display":    ["2.5rem", { lineHeight: "1.1",  letterSpacing: "-0.025em" }],
        "display-lg": ["3.25rem", { lineHeight: "1.05", letterSpacing: "-0.03em" }],
      },
      boxShadow: {
        soft:     "var(--shadow-soft)",
        elevated: "0 4px 16px -4px rgba(0,0,0,0.08), 0 2px 4px -2px rgba(0,0,0,0.04)",
      },
      borderRadius: {
        xl: "0.875rem",  // 14px — cards
        "2xl": "1.25rem", // 20px — modals, sheets
      },
      maxWidth: {
        prose: "72ch",
        reader: "44rem",
        // Standard page content wrapper — tight enough to avoid line-length
        // problems on ultrawides, wide enough that admin tables don't feel cramped
        content: "78rem",
      },
    },
  },
  plugins: [],
} satisfies Config;
