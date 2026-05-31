"use client";

import { useEffect, useRef, useState } from "react";
import videojs from "video.js";
import type Player from "video.js/dist/types/player";
import "video.js/dist/video-js.css";

interface Props {
  src: string;
  type?: string;
  poster?: string;
}

interface QualityLevel {
  index: number;
  height: number;
  bitrate: number;
  enabled: boolean;
}

/**
 * video.js HLS player with a manual quality selector on top of the auto-ABR.
 *
 * Default behavior (Auto): video.js's bundled VHS picks quality based on
 * measured bandwidth. Switches mid-stream as the network changes.
 *
 * Manual override: user can pin a specific quality via the menu we render.
 * This disables ABR until they re-select Auto.
 */
const SPEEDS = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2];

export function VideoPlayer({ src, type = "application/x-mpegURL", poster }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const playerRef = useRef<Player | null>(null);
  const [levels, setLevels] = useState<QualityLevel[]>([]);
  const [selected, setSelected] = useState<number>(-1);   // -1 means Auto
  const [menuOpen, setMenuOpen] = useState(false);
  const [speed, setSpeed] = useState(1);
  const [speedMenuOpen, setSpeedMenuOpen] = useState(false);

  useEffect(() => {
    if (!containerRef.current || playerRef.current) return;

    const el = document.createElement("video-js");
    el.classList.add("vjs-default-skin", "vjs-big-play-centered", "w-full");
    containerRef.current.appendChild(el);

    const player = videojs(el, {
      controls: true,
      preload: "auto",
      responsive: true,
      fluid: true,
      poster,
      playbackRates: SPEEDS,   // also shows up in the native gear menu
      sources: [{ src, type }],
    });
    playerRef.current = player;

    // VHS exposes qualityLevels() — we read them once the manifest is parsed.
    // The list arrives a few hundred ms after src is set.
    type WithQL = Player & { qualityLevels?: () => any };
    const qlPlayer = player as WithQL;
    const refreshLevels = () => {
      if (!qlPlayer.qualityLevels) return;
      const ql = qlPlayer.qualityLevels();
      const arr: QualityLevel[] = [];
      for (let i = 0; i < ql.length; i++) {
        const lvl = ql[i];
        arr.push({
          index: i,
          height: lvl.height || 0,
          bitrate: lvl.bitrate || 0,
          enabled: lvl.enabled,
        });
      }
      // Highest first; visually nicer in menus
      arr.sort((a, b) => b.height - a.height);
      setLevels(arr);
    };

    player.on("loadedmetadata", refreshLevels);
    if (qlPlayer.qualityLevels) {
      qlPlayer.qualityLevels().on("addqualitylevel", refreshLevels);
    }

    return () => { player.dispose(); playerRef.current = null; };
  }, [src, type, poster]);

  const pickQuality = (idx: number) => {
    const player = playerRef.current as (Player & { qualityLevels?: () => any }) | null;
    if (!player?.qualityLevels) return;
    const ql = player.qualityLevels();
    for (let i = 0; i < ql.length; i++) {
      // idx === -1 ↔ Auto: enable all rungs and let ABR pick
      ql[i].enabled = idx === -1 || i === idx;
    }
    setSelected(idx);
    setMenuOpen(false);
  };

  const pickSpeed = (r: number) => {
    const player = playerRef.current;
    if (!player) return;
    player.playbackRate(r);
    setSpeed(r);
    setSpeedMenuOpen(false);
  };

  // YouTube-style keyboard shortcuts when the player has focus or no input does.
  // J/L = seek -10/+10s, K/space = play-pause, ←/→ = -5/+5s, ↑/↓ = volume ±10%,
  // M = mute, F = fullscreen, < = slower, > = faster, 0-9 = jump to N0% position.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const player = playerRef.current;
      if (!player) return;
      const tag = (e.target as HTMLElement | null)?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || (e.target as HTMLElement | null)?.isContentEditable) return;
      const dur = player.duration() || 0;
      switch (e.key.toLowerCase()) {
        case "j": e.preventDefault(); player.currentTime(Math.max(0, (player.currentTime() || 0) - 10)); break;
        case "l": e.preventDefault(); player.currentTime(Math.min(dur, (player.currentTime() || 0) + 10)); break;
        case "k":
        case " ":
          e.preventDefault(); if (player.paused()) void player.play(); else player.pause(); break;
        case "arrowleft":  e.preventDefault(); player.currentTime(Math.max(0, (player.currentTime() || 0) - 5)); break;
        case "arrowright": e.preventDefault(); player.currentTime(Math.min(dur, (player.currentTime() || 0) + 5)); break;
        case "arrowup":   e.preventDefault(); player.volume(Math.min(1, (player.volume() || 0) + 0.1)); break;
        case "arrowdown": e.preventDefault(); player.volume(Math.max(0, (player.volume() || 0) - 0.1)); break;
        case "m": e.preventDefault(); player.muted(!player.muted()); break;
        case "f": e.preventDefault(); if (player.isFullscreen()) void player.exitFullscreen(); else void player.requestFullscreen(); break;
        case ",": {
          // Slower (< key without shift on US keyboards is comma)
          if (e.shiftKey) {
            const next = SPEEDS[Math.max(0, SPEEDS.indexOf(speed) - 1)];
            pickSpeed(next);
          }
          break;
        }
        case ".": {
          // Faster (> key without shift is period)
          if (e.shiftKey) {
            const next = SPEEDS[Math.min(SPEEDS.length - 1, SPEEDS.indexOf(speed) + 1)];
            pickSpeed(next);
          }
          break;
        }
        default:
          // 0-9: jump to N0% position
          if (e.key >= "0" && e.key <= "9") {
            e.preventDefault();
            const frac = parseInt(e.key, 10) / 10;
            player.currentTime(dur * frac);
          }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [speed]);

  // Find which "real" index in the unsorted ql() each row corresponds to.
  // (We sorted `levels` for display; idx field preserves the original index
  // which is what we toggle in ql[].enabled.)

  return (
    <div className="relative">
      <div ref={containerRef} className="aspect-video bg-black rounded overflow-hidden" />

      {/* Speed picker (always shown) — left of the quality picker */}
      <div className="absolute top-2 right-2 z-10 flex items-center gap-1">
        <div className="relative">
          <button
            onClick={() => setSpeedMenuOpen((v) => !v)}
            className="px-2 py-1 text-xs bg-black/70 hover:bg-black/90 text-white rounded backdrop-blur transition"
            title="Playback speed (Shift+, slower, Shift+. faster)"
          >
            {speed}x <span className="opacity-70">▾</span>
          </button>
          {speedMenuOpen && (
            <div className="absolute right-0 mt-1 w-24 bg-black/90 text-white text-xs rounded shadow-lg overflow-hidden backdrop-blur">
              {SPEEDS.map((r) => (
                <button key={r}
                        onClick={() => pickSpeed(r)}
                        className={`block w-full px-3 py-1.5 text-left hover:bg-white/10 ${
                          speed === r ? "bg-white/15 font-semibold" : ""
                        }`}>
                  {r}x
                </button>
              ))}
            </div>
          )}
        </div>

      {levels.length > 1 && (
        <div className="relative">
          <button
            onClick={() => setMenuOpen(!menuOpen)}
            className="px-2 py-1 text-xs bg-black/70 hover:bg-black/90 text-white rounded backdrop-blur transition"
          >
            {selected === -1
              ? "Auto"
              : `${levels.find((l) => l.index === selected)?.height || "?"}p`}
            <span className="ml-1 opacity-70">▾</span>
          </button>

          {menuOpen && (
            <div className="absolute right-0 mt-1 w-40 bg-black/90 text-white text-xs rounded shadow-lg overflow-hidden backdrop-blur">
              <button
                onClick={() => pickQuality(-1)}
                className={`block w-full px-3 py-1.5 text-left hover:bg-white/10 ${
                  selected === -1 ? "bg-white/15 font-semibold" : ""
                }`}
              >
                Auto <span className="text-white/60">(adaptive)</span>
              </button>
              {levels.map((lvl) => (
                <button
                  key={lvl.index}
                  onClick={() => pickQuality(lvl.index)}
                  className={`block w-full px-3 py-1.5 text-left hover:bg-white/10 ${
                    selected === lvl.index ? "bg-white/15 font-semibold" : ""
                  }`}
                >
                  {lvl.height || "?"}p
                  {lvl.bitrate > 0 && (
                    <span className="text-white/60 ml-1">
                      {Math.round(lvl.bitrate / 1000)} kbps
                    </span>
                  )}
                </button>
              ))}
            </div>
          )}
        </div>
      )}
      </div>
    </div>
  );
}
