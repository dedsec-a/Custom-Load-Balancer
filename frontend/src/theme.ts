import { useEffect, useState } from "react";

// Chart colors are passed to Recharts as SVG attributes, so they are resolved
// here per mode rather than through CSS variables. Values come from the
// validated reference palette (slots 1–3 pass all-pairs in both modes).
export interface Palette {
  series: [string, string, string]; // endpoint identity: slot 1, 2, 3
  p95: string; // ordinal pair for latency percentiles (one hue)
  p50: string;
  dim: string; // de-emphasized marks
  grid: string;
  axis: string;
  muted: string;
  text2: string;
  text: string;
  surface: string;
  critical: string; // status: failed requests
  serious: string; // status: shed requests
  categorical: string[]; // identity colours in fixed order (validated, adjacent pairs)
}

const light: Palette = {
  series: ["#2a78d6", "#eb6834", "#1baf7a"],
  p95: "#2a78d6",
  p50: "#86b6ef",
  dim: "#c3c2b7",
  grid: "#e1e0d9",
  axis: "#c3c2b7",
  muted: "#898781",
  text2: "#52514e",
  text: "#0b0b0b",
  surface: "#fcfcfb",
  critical: "#d03b3b",
  serious: "#ec835a",
  categorical: ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300"],
};

const dark: Palette = {
  series: ["#3987e5", "#d95926", "#199e70"],
  p95: "#3987e5",
  p50: "#1c5cab",
  dim: "#52514e",
  grid: "#2c2c2a",
  axis: "#383835",
  muted: "#898781",
  text2: "#c3c2b7",
  text: "#f0efec",
  surface: "#1a1a19",
  critical: "#d03b3b",
  serious: "#ec835a",
  categorical: ["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300"],
};

export function usePalette(): Palette {
  const query = "(prefers-color-scheme: dark)";
  const [isDark, setDark] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const m = window.matchMedia(query);
    const on = (e: MediaQueryListEvent) => setDark(e.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return isDark ? dark : light;
}

export const fmtMs = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(v >= 10000 ? 0 : 1)} s` : `${Math.round(v)} ms`);
export const fmtNum = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : `${Math.round(v)}`);
export const fmtClock = (t: number) =>
  new Date(t).toLocaleTimeString([], { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" });
