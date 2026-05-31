import type { Config } from "tailwindcss";

// Tokens come from CSS variables in globals.css so a single
// data-theme="light|dark" flip retones everything — no class-based dark:
// prefixes needed and no flicker on first paint.
export default {
  content: [
    "./app/**/*.{ts,tsx}",
    "./components/**/*.{ts,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        bg:      "var(--color-bg)",
        panel:   "var(--color-panel)",
        surface: "var(--color-surface)",
        border:  "var(--color-border)",
        muted:   "var(--color-muted)",
        text:    "var(--color-text)",
        "text-soft": "var(--color-text-soft)",
        accent:  "var(--color-link)",   // historical alias — old code uses "accent"
        link:    "var(--color-link)",
        success: "var(--color-success)",
        warning: "var(--color-warning)",
        danger:  "var(--color-danger)",
        codeBg:  "var(--color-codeBg)",
      },
      fontFamily: {
        sans: ["var(--font-sans)", "Inter", "system-ui", "sans-serif"],
        mono: ["var(--font-mono)", "ui-monospace", "JetBrains Mono", "monospace"],
      },
      boxShadow: {
        soft: "var(--shadow-soft)",
      },
      maxWidth: {
        prose: "72ch",
        reader: "44rem",
      },
    },
  },
  plugins: [],
} satisfies Config;
