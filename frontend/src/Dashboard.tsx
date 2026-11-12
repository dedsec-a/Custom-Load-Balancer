import { useCallback, useMemo, useState } from "react";
import { Backends } from "./components/Backends";
import { LoadCurve, TimeChart, type Series } from "./components/Charts";
import { AlgorithmPanel, ResourcesPanel, TrafficPanel } from "./components/Controls";
import { deriveEvents, segments, totals, useLive } from "./live";
import { Link } from "./router";
import { fmtClock, fmtMs, fmtNum, usePalette } from "./theme";
import { algoLabel, type Point } from "./types";

const WINDOWS = [
  { s: 60, label: "1 min" },
  { s: 120, label: "2 min" },
  { s: 300, label: "5 min" },
  { s: 600, label: "10 min" },
];

export default function Dashboard() {
  const { points, state, conn, refresh } = useLive();
  const pal = usePalette();
  const [win, setWin] = useState(120);
  const [error, setError] = useState<string | null>(null);

  const onError = useCallback((m: string) => {
    setError(m);
    window.setTimeout(() => setError((cur) => (cur === m ? null : cur)), 6000);
  }, []);
  const onChanged = useCallback(() => {
    setError(null);
    refresh();
  }, [refresh]);

  const view = useMemo(() => {
    if (!points.length) return points;
    const from = points[points.length - 1].t - win * 1000;
    return points.filter((p) => p.t >= from);
  }, [points, win]);
  const events = useMemo(() => deriveEvents(view), [view]);
  const latest: Point | undefined = points[points.length - 1];

  // Endpoint identity is fixed by sorted path name so colors never repaint.
  const paths = useMemo(() => {
    const set = new Set(Object.keys(state?.load.mix ?? {}));
    for (const p of view) for (const k of Object.keys(p.paths ?? {})) set.add(k);
    return [...set].sort().slice(0, 3);
  }, [state?.load.mix, view]);

  const latency: Series[] = [
    { key: "p95", name: "p95", color: pal.p95, get: (p) => (p.requests ? p.p95_ms : null) },
    { key: "p50", name: "p50", color: pal.p50, get: (p) => (p.requests ? p.p50_ms : null) },
  ];
  const byPath: Series[] = paths.map((path, i) => ({
    key: `path${i}`,
    name: path,
    color: pal.series[i],
    get: (p) => {
      const x = p.paths?.[path];
      return x && x.requests ? x.p95_ms : null;
    },
  }));

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>
            <Link to="/" className="brand-link">Load Balancer Control</Link>
          </h1>
          <span className="sub">L7 · round robin / FCFS / SJF</span>
        </div>
        <div className="top-actions">
          <Link to="/experiments" className="top-link">Experiments</Link>
          <Link to="/#metrics" className="top-link">What do these numbers mean?</Link>
          <ConnBadge conn={conn} />
        </div>
      </header>

      {state?.experiment && (
        <div className="banner info" role="status">
          <span aria-hidden>⏱</span> An experiment is running (step {state.experiment.step} of {state.experiment.steps},
          about {Math.ceil(state.experiment.eta_s / 60)} min left). Controls are locked so it stays fair.
          <Link to="/experiments" className="btn sm">View</Link>
        </div>
      )}

      {error && (
        <div className="banner" role="alert">
          <span aria-hidden>⚠</span> {error}
          <button className="btn sm" onClick={() => setError(null)}>Dismiss</button>
        </div>
      )}

      <div className="layout">
        <aside className="sidebar">
          <TrafficPanel state={state} onError={onError} onChanged={onChanged} />
          <ResourcesPanel state={state} onError={onError} onChanged={onChanged} />
          <AlgorithmPanel state={state} onError={onError} onChanged={onChanged} />
        </aside>

        <main className="main">
          <StatTiles p={latest} />

          <div className="filter-row">
            <div className="segmented small" role="radiogroup" aria-label="Time window">
              {WINDOWS.map((w) => (
                <button
                  key={w.s}
                  role="radio"
                  aria-checked={win === w.s}
                  className={win === w.s ? "seg on" : "seg"}
                  onClick={() => setWin(w.s)}
                >
                  {w.label}
                </button>
              ))}
            </div>
            <EventStrip events={events} />
          </div>

          <div className="chart-grid">
            <TimeChart
              title="Response time"
              note="End to end through the LB, including time queued"
              points={view}
              series={latency}
              events={events}
              pal={pal}
              fmt={fmtMs}
            />
            <TimeChart
              title="Throughput (requests/s)"
              points={view}
              series={[{ key: "rps", name: "Requests/s", color: pal.p95, get: (p) => p.rps }]}
              events={events}
              pal={pal}
              fmt={fmtNum}
            />
            <TimeChart
              title="p95 by endpoint"
              note="SJF should pull the short endpoint down and push the long one up"
              points={view}
              series={byPath}
              events={events}
              pal={pal}
              fmt={fmtMs}
            />
            <TimeChart
              title="Requests waiting in the LB queue"
              note="Always 0 under round robin: it forwards everything at once"
              points={view}
              series={[{ key: "q", name: "Queued", color: pal.p95, get: (p) => p.queue_len }]}
              events={events}
              pal={pal}
              fmt={fmtNum}
            />
            <TimeChart
              title="Failed, shed and retried requests"
              note="Shed = turned away by the bounded queue. Retried = moved to another backend after a failure."
              points={view}
              series={[
                { key: "shed", name: "Shed", color: pal.serious, get: (p) => p.shed ?? 0 },
                { key: "err", name: "Failed", color: pal.critical, get: (p) => p.errors },
                { key: "retry", name: "Retried", color: pal.p50, get: (p) => p.retries ?? 0 },
              ]}
              events={events}
              pal={pal}
              fmt={fmtNum}
            />
            <TimeChart
              title="Job size: estimated vs measured"
              note="SJF orders the queue by the estimate (a moving average). It should track the measured work time."
              points={view}
              series={paths.flatMap((path, i) => [
                { key: `est${i}`, name: `${path} est.`, color: pal.series[i], get: (p: Point) => p.paths?.[path]?.est_ms || null },
                {
                  key: `work${i}`,
                  name: `${path} measured`,
                  color: pal.series[i],
                  dashed: true,
                  get: (p: Point) => p.paths?.[path]?.work_ms || null,
                },
              ])}
              events={events}
              pal={pal}
              fmt={fmtMs}
            />
            <LoadCurve points={view} pal={pal} />
          </div>

          {state && (
            <Backends locked={state.limits?.lock_backends} maxFault={state.limits?.max_fault_s || 600} backends={state.backends} points={view} pal={pal} onError={onError} onChanged={onChanged} />
          )}
          <RunLog points={points} />
        </main>
      </div>
    </div>
  );
}

function ConnBadge({ conn }: { conn: string }) {
  const text = conn === "live" ? "Live" : conn === "connecting" ? "Connecting…" : "Disconnected";
  return (
    <span className={`conn ${conn}`}>
      <span aria-hidden>{conn === "live" ? "●" : conn === "connecting" ? "◌" : "✕"}</span> {text}
    </span>
  );
}

function StatTiles({ p }: { p?: Point }) {
  const t = p ? totals(p) : { cores: 0, db: 0, backends: 0 };
  const tiles = [
    { label: "Users", value: p ? fmtNum(p.users) : "—" },
    { label: "Requests/s", value: p ? fmtNum(p.rps) : "—" },
    { label: "p95 latency", value: p && p.requests ? fmtMs(p.p95_ms) : "—" },
    { label: "Queued at LB", value: p ? fmtNum(p.queue_len) : "—" },
    { label: "Utilization", value: p ? `${Math.round(p.utilization * 100)}%` : "—", sub: "in flight + queued ÷ slots" },
    {
      label: "Failed / shed /s",
      value: p ? `${fmtNum(p.errors)} / ${fmtNum(p.shed ?? 0)}` : "—",
      sub: p && p.retries ? `${p.retries} retried elsewhere` : "retries hide most backend failures",
      bad: !!p && (p.errors > 0 || (p.shed ?? 0) > 0),
    },
    { label: "Capacity", value: p ? `${t.cores} cores` : "—", sub: p ? `${t.db} DB conns · ${t.backends} backends` : "" },
    {
      label: "Algorithm",
      value: p ? algoLabel(p.effective) : "—",
      sub: p?.algorithm === "auto" ? "chosen by auto" : "set manually",
    },
  ];
  return (
    <div className="tiles">
      {tiles.map((x) => (
        <div className={x.bad ? "tile bad" : "tile"} key={x.label}>
          <span className="tile-label">{x.label}</span>
          <span className="tile-value">
            {x.bad && <span aria-hidden>⚠ </span>}
            {x.value}
          </span>
          {x.sub && <span className="tile-sub">{x.sub}</span>}
        </div>
      ))}
    </div>
  );
}

function EventStrip({ events }: { events: { t: number; label: string }[] }) {
  if (!events.length) return <span className="muted small">Changes you make show up here and as vertical lines on the charts.</span>;
  return (
    <ol className="events" aria-label="Recent changes">
      {events
        .slice(-4)
        .reverse()
        .map((e) => (
          <li key={e.t}>
            <time>{fmtClock(e.t)}</time> {e.label}
          </li>
        ))}
    </ol>
  );
}

function RunLog({ points }: { points: Point[] }) {
  const rows = useMemo(() => segments(points).filter((s) => s.users > 0 && s.seconds >= 3).reverse().slice(0, 12), [points]);
  return (
    <section className="card">
      <header className="card-head">
        <div>
          <h3>Results by configuration</h3>
          <p className="note">A new row starts whenever users, resources or the algorithm change. Medians skip the first second of each row.</p>
        </div>
      </header>
      {rows.length === 0 ? (
        <p className="empty-inline">Run some traffic for a few seconds to fill this table.</p>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Started</th>
                <th className="r">Seconds</th>
                <th className="r">Users</th>
                <th className="r">Cores</th>
                <th className="r">DB pool</th>
                <th>Algorithm</th>
                <th className="r">Requests/s</th>
                <th className="r">p95 (median)</th>
                <th className="r">Errors</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((s) => (
                <tr key={s.start}>
                  <td>{fmtClock(s.start)}</td>
                  <td className="r">{s.seconds}</td>
                  <td className="r">{s.users}</td>
                  <td className="r">{s.cores}</td>
                  <td className="r">{s.db}</td>
                  <td>
                    {algoLabel(s.effective)}
                    {s.effective === "sjf" && s.aging != null && <span className="muted"> aging {s.aging}</span>}
                    {s.algorithm === "auto" && <span className="muted"> (auto)</span>}
                  </td>
                  <td className="r">{fmtNum(s.rps)}</td>
                  <td className="r">{fmtMs(s.p95)}</td>
                  <td className="r">{s.errors}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
