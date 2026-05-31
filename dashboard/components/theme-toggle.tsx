"use client";

// Theme toggle button — flips data-theme on <html>, persists to
// localStorage. Initial value is set by the inline script in
// app/layout.tsx so we never see a flash of the wrong theme.

import { useEffect, useState } from "react";
import { Sun, Moon } from "lucide-react";

type Theme = "light" | "dark";

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>("dark");

  useEffect(() => {
    const t = (document.documentElement.getAttribute("data-theme") as Theme) || "dark";
    setTheme(t);
  }, []);

  const flip = () => {
    const next: Theme = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    try { localStorage.setItem("ps3_theme", next); } catch {}
    setTheme(next);
  };

  return (
    <button
      onClick={flip}
      className="inline-flex items-center justify-center w-9 h-9 rounded-full border border-border text-muted hover:text-text hover:border-text/40 transition-colors"
      title={theme === "dark" ? "Switch to light theme" : "Switch to dark theme"}
      aria-label="toggle theme"
    >
      {theme === "dark" ? <Sun size={15} /> : <Moon size={15} />}
    </button>
  );
}
