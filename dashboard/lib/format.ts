export function formatBytes(n: number): string {
  if (n === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return (n / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 2) + " " + units[i];
}

export function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

export function classify(key: string, contentType?: string): "video" | "audio" | "image" | "other" {
  const ct = (contentType || "").toLowerCase();
  if (ct.startsWith("video/")) return "video";
  if (ct.startsWith("audio/")) return "audio";
  if (ct.startsWith("image/")) return "image";
  const ext = key.split(".").pop()?.toLowerCase() || "";
  if (["mp4","mov","mkv","avi","webm","m4v"].includes(ext)) return "video";
  if (["mp3","wav","flac","aac","ogg","m4a","opus"].includes(ext)) return "audio";
  if (["jpg","jpeg","png","gif","webp","avif","tiff","bmp"].includes(ext)) return "image";
  return "other";
}

import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
