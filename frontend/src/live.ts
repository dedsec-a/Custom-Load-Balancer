import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
import { algoLabel, type Point, type State } from "./types";

const KEEP = 600; // seconds of history kept in the browser (matches the LB)

export type Conn = "connecting" | "live" | "down";

/** History + live SSE stream of 1-second points, plus polled control state. */
export function useLive() {
  const [points, setPoints] = useState<Point[]>([]);
  const [state, setState] = useState<State | null>(null);
  const [conn, setConn] = useState<Conn>("connecting");
  const lastSeen = useRef(0);

  const refresh = useCallback(async () => {
    try {
      setState(await api.state());
    } catch {
      /* stream status already shows the outage */
    }
  }, []);

  useEffect(() => {
    let alive = true;
    const add = (incoming: Point[]) => {
      const fresh = incoming.filter((p) => p.t > lastSeen.current);
      if (!fresh.length) return;
      lastSeen.current = fresh[fresh.length - 1].t;
      setPoints((prev) => {
        const next = prev.concat(fresh);
        return next.length > KEEP ? next.slice(next.length - KEEP) : next;
      });
    };

    api.metrics(KEEP).then((h) => alive && add(h)).catch(() => {});
    refresh();

    const es = new EventSource(api.streamURL);
    es.onopen = () => setConn("live");
    es.onerror = () => setConn(es.readyState === EventSource.CLOSED ? "down" : "connecting");
    es.addEventListener("point", (e) => {
      setConn("live");
      add([JSON.parse((e as MessageEvent).data) as Point]);
    });
    const poll = setInterval(refresh, 2000);
    return () => {
      alive = false;
      es.close();
      clearInterval(poll);
    };
  }, [refresh]);

  return { points, state, setState, conn, refresh };
}

// ---- derived data ---------------------------------------------------------

export const totals = (p: Point) => {
  let cores = 0, db = 0;
  for (const b of p.backends ?? []) {
    cores += b.cores;
    db += b.db_pool;
  }
  return { cores, db, backends: p.backends?.length ?? 0 };
};

export interface LBEvent {
  t: number;
  label: string;
}

/** Changes worth marking on the charts, inferred from consecutive points, so
 * they also show changes made with curl or by auto mode, and survive reloads. */
export function deriveEvents(points: Point[]): LBEvent[] {
  const out: LBEvent[] = [];
  for (let i = 1; i < points.length; i++) {
    const a = points[i - 1], b = points[i];
    const parts: string[] = [];
    if (a.users !== b.users) parts.push(`Users ${a.users} → ${b.users}`);
    const ta = totals(a), tb = totals(b);
    if (ta.backends !== tb.backends) parts.push(`Backends ${ta.backends} → ${tb.backends}`);
    else {
      if (ta.cores !== tb.cores) parts.push(`Cores ${ta.cores} → ${tb.cores}`);
      if (ta.db !== tb.db) parts.push(`DB pool ${ta.db} → ${tb.db}`);
    }
    if (a.effective !== b.effective) {
      parts.push(`${algoLabel(a.effective)} → ${algoLabel(b.effective)}${b.algorithm === "auto" ? " (auto)" : ""}`);
    } else if (a.algorithm !== b.algorithm) {
      parts.push(`Mode ${algoLabel(a.algorithm)} → ${algoLabel(b.algorithm)}`);
    }
    for (const nb of b.backends ?? []) {
      const ob = a.backends?.find((x) => x.name === nb.name);
      if (!ob) continue;
      if (ob.healthy && !nb.healthy) parts.push(`${nb.name} ${nb.down ? "down" : nb.ejected ? "ejected" : "unhealthy"}`);
      else if (!ob.healthy && nb.healthy) parts.push(`${nb.name} back`);
      if (!ob.draining && nb.draining) parts.push(`${nb.name} draining`);
    }
    if (parts.length) out.push({ t: b.t, label: parts.join(" · ") });
  }
  return out;
}

export interface Segment {
  start: number;
  end: number;
  users: number;
  cores: number;
  db: number;
  algorithm: string;
  effective: string;
  aging?: number;
  rps: number;
  p95: number;
  errors: number;
  seconds: number;
}

const median = (xs: number[]) => {
  if (!xs.length) return 0;
  const s = [...xs].sort((x, y) => x - y);
  return s[Math.floor(s.length / 2)];
};

/** Splits history into runs with one configuration each, for the results table. */
export function segments(points: Point[]): Segment[] {
  const out: Segment[] = [];
  let run: Point[] = [];
  const key = (p: Point) => {
    const t = totals(p);
    return `${p.users}|${t.cores}|${t.db}|${p.algorithm}|${p.effective}|${p.effective === "sjf" ? p.aging ?? "" : ""}`;
  };
  const flush = () => {
    if (!run.length) return;
    const first = run[0], t = totals(first);
    // Skip the first second of a run: it straddles the change.
    const steady = run.length > 2 ? run.slice(1) : run;
    const busy = steady.filter((p) => p.requests > 0);
    out.push({
      start: first.t,
      end: run[run.length - 1].t,
      users: first.users,
      cores: t.cores,
      db: t.db,
      algorithm: first.algorithm,
      effective: first.effective,
      aging: first.aging,
      rps: steady.reduce((s, p) => s + p.rps, 0) / steady.length,
      p95: median(busy.map((p) => p.p95_ms)),
      errors: steady.reduce((s, p) => s + p.errors, 0),
      seconds: run.length,
    });
  };
  for (const p of points) {
    if (run.length && key(run[run.length - 1]) !== key(p)) {
      flush();
      run = [];
    }
    run.push(p);
  }
  flush();
  return out;
}
