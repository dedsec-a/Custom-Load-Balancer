import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Bar,
  BarChart,
  CartesianGrid,
  ComposedChart,
  ErrorBar,
  Line,
  ResponsiveContainer,
  Scatter,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { api } from "./api";
import { Legend } from "./components/Charts";
import { Link } from "./router";
import { fmtMs, fmtNum, usePalette, type Palette } from "./theme";
import type { Experiment, ExperimentSpec, State, TheoryPoint, Variant, VariantSummary } from "./types";

// ---- presets -----------------------------------------------------------------

const ALL_VARIANTS: Variant[] = [
  { name: "Round robin", algorithm: "round_robin" },
  { name: "Least conn", algorithm: "least_conn" },
  { name: "FCFS", algorithm: "fcfs" },
  { name: "SJF", algorithm: "sjf", aging: 0.02 },
];

const AGING_VARIANTS: Variant[] = [
  { name: "FCFS", algorithm: "fcfs" },
  { name: "SJF, no aging", algorithm: "sjf", aging: 0 },
  { name: "SJF, aging 0.02", algorithm: "sjf", aging: 0.02 },
  { name: "SJF, aging 0.1", algorithm: "sjf", aging: 0.1 },
];

const plannedSeconds = (s: ExperimentSpec) => {
  const steps = s.kind === "theory" ? (s.utilizations?.length ?? 6) : (s.variants?.length ?? 4) * s.repeats;
  return steps * (s.duration_s + s.warmup_s + 3);
};
const fmtDuration = (s: number) => (s >= 60 ? `${Math.floor(s / 60)} min ${s % 60 ? `${s % 60} s` : ""}` : `${s} s`);
const pct = (x: number) => `${Math.round(x * 100)}%`;

// ---- page ----------------------------------------------------------------------

export default function Experiments() {
  const pal = usePalette();
  const [list, setList] = useState<Experiment[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [detail, setDetail] = useState<Experiment | null>(null);
  const [state, setState] = useState<State | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [l, s] = await Promise.all([api.experiments(), api.state()]);
      setList(l);
      setState(s);
      // Open on the latest finished result; a run started from this page selects itself.
      setSelected((cur) => cur ?? l.find((e) => e.status === "done")?.id ?? l[0]?.id ?? null);
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 2000);
    return () => clearInterval(id);
  }, [refresh]);

  const running = list.find((e) => e.status === "running");
  useEffect(() => {
    if (!selected) return;
    let alive = true;
    api.experiment(selected).then((e) => alive && setDetail(e)).catch(() => {});
    return () => {
      alive = false;
    };
    // Re-fetch the selected one whenever the list changes (progress, completion).
  }, [selected, list]);

  const start = async (spec: ExperimentSpec) => {
    setError(null);
    try {
      const e = await api.startExperiment(spec);
      setSelected(e.id);
      refresh();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const maxUsers = state?.limits?.max_users || 2000;
  const maxS = state?.limits?.max_experiment_s || 0;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>
            <Link to="/" className="brand-link">Load Balancer</Link>
          </h1>
          <span className="sub">Experiments</span>
        </div>
        <div className="top-actions">
          <Link to="/#algorithms" className="top-link">How the algorithms work</Link>
          <Link to="/dashboard" className="lp-btn small">Dashboard</Link>
        </div>
      </header>

      <main className="exp-main">
        <section className="exp-intro">
          <h2>Controlled experiments</h2>
          <p>
            The dashboard shows one moment. An experiment answers a question fairly: every configuration gets the
            <b> same load</b>, the <b>same request sequence</b> (a fixed random seed), the same duration and a warm-up,
            the run order rotates between repeats, and results are reported as the <b>median of several runs with their
            range</b>, so a lucky run can't decide the result. While one runs, the dashboard's controls are locked.
          </p>
          {maxS > 0 && <p className="hint">This server allows experiments up to {fmtDuration(maxS)}.</p>}
        </section>

        {error && (
          <div className="banner" role="alert">
            <span aria-hidden>⚠</span> {error}
            <button className="btn sm" onClick={() => setError(null)}>Dismiss</button>
          </div>
        )}

        {running && <RunningCard e={running} onCancel={() => api.cancelExperiment(running.id).then(refresh)} />}

        <div className="exp-presets">
          <ComparePreset
            title="Compare the algorithms"
            blurb="Round robin, least connections, FCFS and SJF at the same overload. Which endpoint gets faster, and what does it cost?"
            variants={ALL_VARIANTS}
            maxUsers={maxUsers}
            maxS={maxS}
            disabled={!!running}
            onRun={start}
          />
          <ComparePreset
            title="SJF starvation: aging before / after"
            blurb="Pure SJF can make long jobs wait for a very long time. Aging lets them catch up. Compare the max wait of /search."
            variants={AGING_VARIANTS}
            maxUsers={maxUsers}
            maxS={maxS}
            disabled={!!running}
            onRun={start}
          />
          <TheoryPreset maxS={maxS} disabled={!!running} onRun={start} />
        </div>

        {detail && <Results e={detail} pal={pal} />}

        <History list={list} selected={selected} onSelect={setSelected} />
      </main>
    </div>
  );
}

// ---- running banner ------------------------------------------------------------

function RunningCard({ e, onCancel }: { e: Experiment; onCancel: () => void }) {
  const done = e.steps ? (e.step - 1) / e.steps : 0;
  return (
    <section className="card exp-running" aria-live="polite">
      <div className="exp-running-head">
        <b>Running: {e.spec.kind === "theory" ? "theory check" : "comparison"}</b>
        <span className="muted">
          step {e.step} of {e.steps} · {e.current} · about {fmtDuration(e.eta_s)} left
        </span>
        <button className="btn sm danger" onClick={onCancel}>Cancel</button>
      </div>
      <div className="meter-track big" role="progressbar" aria-valuenow={Math.round(done * 100)} aria-valuemin={0} aria-valuemax={100}>
        <div className="meter-fill" style={{ width: `${Math.max(2, done * 100)}%` }} />
      </div>
    </section>
  );
}

// ---- presets -------------------------------------------------------------------

function ComparePreset(props: {
  title: string;
  blurb: string;
  variants: Variant[];
  maxUsers: number;
  maxS: number;
  disabled: boolean;
  onRun: (s: ExperimentSpec) => void;
}) {
  const [users, setUsers] = useState(Math.min(150, props.maxUsers));
  const [duration, setDuration] = useState(30);
  const [repeats, setRepeats] = useState(3);
  const [seed, setSeed] = useState(42);
  const [chosen, setChosen] = useState(props.variants.map((v) => v.name));
  const spec: ExperimentSpec = {
    kind: "compare",
    variants: props.variants.filter((v) => chosen.includes(v.name)),
    users,
    duration_s: duration,
    warmup_s: 5,
    repeats,
    seed,
  };
  const secs = plannedSeconds(spec);
  const tooLong = props.maxS > 0 && secs > props.maxS;
  return (
    <section className="card exp-preset">
      <h3>{props.title}</h3>
      <p className="note">{props.blurb}</p>
      <fieldset className="exp-variants">
        <legend className="label">Configurations</legend>
        {props.variants.map((v) => (
          <label key={v.name} className="check">
            <input
              type="checkbox"
              checked={chosen.includes(v.name)}
              onChange={(e) => setChosen((c) => (e.target.checked ? [...c, v.name] : c.filter((x) => x !== v.name)))}
            />
            {v.name}
          </label>
        ))}
      </fieldset>
      <div className="exp-grid">
        <label>
          Users
          <input className="num" type="number" min={1} max={props.maxUsers} value={users} onChange={(e) => setUsers(Math.min(props.maxUsers, Number(e.target.value) || 1))} />
        </label>
        <label>
          Seconds per run
          <select value={duration} onChange={(e) => setDuration(Number(e.target.value))}>
            {[15, 30, 60, 90].map((d) => (
              <option key={d} value={d}>{d}</option>
            ))}
          </select>
        </label>
        <label>
          Repeats
          <select value={repeats} onChange={(e) => setRepeats(Number(e.target.value))}>
            {[1, 3, 5].map((d) => (
              <option key={d} value={d}>{d}</option>
            ))}
          </select>
        </label>
        <label>
          Seed
          <input className="num" type="number" min={1} value={seed} onChange={(e) => setSeed(Number(e.target.value) || 1)} />
        </label>
      </div>
      <div className="exp-run">
        <span className={tooLong ? "bad-text" : "muted"}>
          {chosen.length < 2 ? "Pick at least two" : `Takes about ${fmtDuration(secs)}${tooLong ? " (over this server's limit)" : ""}`}
        </span>
        <button className="btn primary" disabled={props.disabled || chosen.length < 2 || tooLong} onClick={() => props.onRun(spec)}>
          Run
        </button>
      </div>
    </section>
  );
}

function TheoryPreset({ maxS, disabled, onRun }: { maxS: number; disabled: boolean; onRun: (s: ExperimentSpec) => void }) {
  const [duration, setDuration] = useState(45);
  const spec: ExperimentSpec = {
    kind: "theory",
    path: "/search",
    utilizations: [0.3, 0.5, 0.7, 0.8, 0.9, 0.95],
    duration_s: duration,
    warmup_s: 5,
    repeats: 1,
    seed: 7,
  };
  const secs = plannedSeconds(spec);
  const tooLong = maxS > 0 && secs > maxS;
  return (
    <section className="card exp-preset">
      <h3>Theory vs reality (M/M/c)</h3>
      <p className="note">
        Poisson arrivals at 30–95% utilization into one FIFO queue in front of every CPU core, the textbook M/x/c system.
        Measured queue waits are compared with Erlang C (M/M/c) and its correction for this server's near-constant job
        times (M/G/c).
      </p>
      <div className="exp-grid">
        <label>
          Seconds per step
          <select value={duration} onChange={(e) => setDuration(Number(e.target.value))}>
            {[20, 45, 90].map((d) => (
              <option key={d} value={d}>{d}</option>
            ))}
          </select>
        </label>
        <label>
          Endpoint
          <input className="num wide" value="/search (100 ms CPU)" readOnly />
        </label>
      </div>
      <div className="exp-run">
        <span className={tooLong ? "bad-text" : "muted"}>Takes about {fmtDuration(secs)}</span>
        <button className="btn primary" disabled={disabled || tooLong} onClick={() => onRun(spec)}>
          Run
        </button>
      </div>
    </section>
  );
}

// ---- results -------------------------------------------------------------------

function Results({ e, pal }: { e: Experiment; pal: Palette }) {
  return (
    <section className="exp-results">
      <header className="exp-results-head">
        <div>
          <p className="lp-kicker">{e.spec.kind === "theory" ? "Theory vs reality" : "Comparison"} · {new Date(e.started).toLocaleString()}</p>
          <h2 className="exp-finding">{e.finding || statusLine(e)}</h2>
          <p className="note">{specLine(e)}</p>
        </div>
        <Downloads e={e} />
      </header>
      {e.spec.kind === "compare" && e.summary && e.summary.length > 0 && <CompareResults sum={e.summary} pal={pal} />}
      {e.spec.kind === "theory" && e.theory && e.theory.length > 0 && <TheoryResults pts={e.theory} pal={pal} durationS={e.spec.duration_s} />}
      {e.status === "running" && !e.summary?.length && !e.theory?.length && <p className="empty-inline">Results appear here when it finishes.</p>}
    </section>
  );
}

const statusLine = (e: Experiment) =>
  e.status === "running" ? "Running…" : e.status === "failed" ? `Failed: ${e.error}` : e.status === "cancelled" ? "Cancelled" : "Done";

function specLine(e: Experiment) {
  const s = e.spec;
  if (s.kind === "theory")
    return `${s.path}, open-loop Poisson arrivals, ${s.duration_s} s per step after ${s.warmup_s} s warm-up, seed ${s.seed}.`;
  const mix = Object.entries(s.mix ?? {})
    .map(([p, w]) => `${p} ${w}`)
    .join(", ");
  return `${s.users} users, mix ${mix}, ${s.duration_s} s per run after ${s.warmup_s} s warm-up, ${s.repeats} repeats (median and range), seed ${s.seed}.`;
}

type Metric = "p95_ms" | "p99_ms" | "max_ms";
const METRICS: { id: Metric; label: string }[] = [
  { id: "p95_ms", label: "p95" },
  { id: "p99_ms", label: "p99" },
  { id: "max_ms", label: "Worst (max)" },
];

function CompareResults({ sum, pal }: { sum: VariantSummary[]; pal: Palette }) {
  const [metric, setMetric] = useState<Metric>("p95_ms");
  const paths = useMemo(() => [...new Set(sum.flatMap((v) => Object.keys(v.paths)))].sort(), [sum]);
  const colors = sum.map((_, i) => pal.categorical[i % pal.categorical.length]);
  const rows = paths.map((path) => {
    const r: Record<string, string | number | number[]> = { path };
    sum.forEach((v, i) => {
      const s = v.paths[path]?.[metric];
      if (!s) return;
      r[`v${i}`] = s.median;
      r[`e${i}`] = [s.median - s.min, s.max - s.median];
    });
    return r;
  });
  const tpRows = sum.map((v, i) => ({
    name: v.variant,
    v: v.throughput.median,
    e: [v.throughput.median - v.throughput.min, v.throughput.max - v.throughput.median],
    i,
  }));

  return (
    <>
      <div className="filter-row">
        <div className="segmented small" role="radiogroup" aria-label="Latency measure">
          {METRICS.map((m) => (
            <button key={m.id} role="radio" aria-checked={metric === m.id} className={metric === m.id ? "seg on" : "seg"} onClick={() => setMetric(m.id)}>
              {m.label}
            </button>
          ))}
        </div>
        <span className="muted small">Bars = median of the repeats; whiskers = the fastest and slowest run.</span>
      </div>
      <div className="chart-grid">
        <section className="card chart-card wide">
          <header className="card-head">
            <div>
              <h3>{METRICS.find((m) => m.id === metric)?.label} latency by endpoint</h3>
              <p className="note">Lower is better. Look for the endpoint a scheduler helps and the one it hurts.</p>
            </div>
            <Legend items={sum.map((v, i) => ({ name: v.variant, color: colors[i] }))} />
          </header>
          <div style={{ height: 300 }}>
            <ResponsiveContainer>
              <BarChart data={rows} margin={{ top: 8, right: 16, left: 0, bottom: 0 }} barGap={2} barCategoryGap="22%">
                <CartesianGrid vertical={false} stroke={pal.grid} />
                <XAxis dataKey="path" stroke={pal.axis} tick={{ fill: pal.text2, fontSize: 12 }} tickLine={false} />
                <YAxis width={56} tickFormatter={fmtMs} stroke={pal.axis} tick={{ fill: pal.muted, fontSize: 11 }} tickLine={false} axisLine={false} />
                <Tooltip content={<SpreadTip sum={sum} metric={metric} />} cursor={{ fill: pal.grid, fillOpacity: 0.4 }} isAnimationActive={false} />
                {sum.map((v, i) => (
                  <Bar key={v.variant} dataKey={`v${i}`} name={v.variant} fill={colors[i]} radius={[4, 4, 0, 0]} isAnimationActive={false}>
                    <ErrorBar dataKey={`e${i}`} width={5} strokeWidth={1.5} stroke={pal.text2} />
                  </Bar>
                ))}
              </BarChart>
            </ResponsiveContainer>
          </div>
        </section>
        <section className="card chart-card">
          <header className="card-head">
            <div>
              <h3>Throughput (successful requests/s)</h3>
              <p className="note">Higher is better.</p>
            </div>
          </header>
          <div style={{ height: 220 }}>
            <ResponsiveContainer>
              <BarChart data={tpRows} layout="vertical" margin={{ top: 4, right: 40, left: 8, bottom: 0 }}>
                <CartesianGrid horizontal={false} stroke={pal.grid} />
                <XAxis type="number" tickFormatter={fmtNum} stroke={pal.axis} tick={{ fill: pal.muted, fontSize: 11 }} tickLine={false} />
                <YAxis type="category" dataKey="name" width={110} stroke={pal.axis} tick={{ fill: pal.text2, fontSize: 12 }} tickLine={false} />
                <Tooltip
                  content={({ active, payload }) =>
                    active && payload?.length ? (
                      <div className="tip">
                        <div className="tip-row">
                          <span>{(payload[0].payload as { name: string }).name}</span>
                          <b>{fmtNum(payload[0].value as number)} req/s</b>
                        </div>
                      </div>
                    ) : null
                  }
                  cursor={{ fill: pal.grid, fillOpacity: 0.4 }}
                  isAnimationActive={false}
                />
                <Bar dataKey="v" radius={[0, 4, 4, 0]} isAnimationActive={false} shape={(p: unknown) => <ColoredBar {...(p as BarShape)} colors={colors} />}>
                  <ErrorBar dataKey="e" width={5} strokeWidth={1.5} stroke={pal.text2} direction="x" />
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
        </section>
        <section className="card chart-card">
          <header className="card-head">
            <div>
              <h3>Longest wait in the LB queue, /search</h3>
              <p className="note">The starvation check: how long the longest job sat in the queue in the worst run.</p>
            </div>
          </header>
          <ul className="exp-starve">
            {sum.map((v, i) => {
              const s = v.paths["/search"]?.max_wait_ms;
              const noQueue = v.algorithm === "round_robin" || v.algorithm === "least_conn";
              return (
                <li key={v.variant}>
                  <span className="swatch" style={{ background: colors[i] }} />
                  <span>{v.variant}</span>
                  {noQueue ? <span className="muted">no LB queue: waits happen inside the backends</span> : <b>{s ? fmtMs(s.max) : "—"}</b>}
                </li>
              );
            })}
          </ul>
        </section>
      </div>
      <CompareTable sum={sum} paths={paths} />
    </>
  );
}

type BarShape = { x: number; y: number; width: number; height: number; payload: { i: number } };
function ColoredBar({ x, y, width, height, payload, colors }: BarShape & { colors: string[] }) {
  const r = Math.min(4, height / 2, width / 2);
  return <path d={`M${x},${y}h${width - r}a${r},${r} 0 0 1 ${r},${r}v${height - 2 * r}a${r},${r} 0 0 1 -${r},${r}h-${width - r}z`} fill={colors[payload.i]} />;
}

function SpreadTip({ active, payload, label, sum, metric }: { active?: boolean; payload?: { dataKey: string; color: string }[]; label?: string; sum: VariantSummary[]; metric: Metric }) {
  if (!active || !payload?.length || !label) return null;
  return (
    <div className="tip">
      <div className="tip-time">{label}</div>
      {sum.map((v, i) => {
        const s = v.paths[label]?.[metric];
        if (!s) return null;
        return (
          <div className="tip-row" key={v.variant}>
            <span className="swatch" style={{ background: payload.find((p) => p.dataKey === `v${i}`)?.color }} />
            <span>{v.variant}</span>
            <b>
              {fmtMs(s.median)} <span className="muted">({fmtMs(s.min)}–{fmtMs(s.max)})</span>
            </b>
          </div>
        );
      })}
    </div>
  );
}

const sp = (s?: { median: number; min: number; max: number }, f: (x: number) => string = fmtMs) =>
  s ? (
    <>
      {f(s.median)} <span className="muted">{s.min === s.max ? "" : `(${f(s.min)}–${f(s.max)})`}</span>
    </>
  ) : (
    "—"
  );

function CompareTable({ sum, paths }: { sum: VariantSummary[]; paths: string[] }) {
  return (
    <section className="card">
      <header className="card-head">
        <div>
          <h3>All numbers</h3>
          <p className="note">Median across repeats, with the range in brackets.</p>
        </div>
      </header>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Configuration</th>
              <th className="r">Requests/s</th>
              <th className="r">p50</th>
              <th className="r">p95</th>
              <th className="r">p99</th>
              {paths.map((p) => (
                <th className="r" key={p}>
                  {p} p95 / p99
                </th>
              ))}
              <th className="r">Failed + shed</th>
            </tr>
          </thead>
          <tbody>
            {sum.map((v) => (
              <tr key={v.variant}>
                <td>{v.variant}</td>
                <td className="r">{sp(v.throughput, fmtNum)}</td>
                <td className="r">{sp(v.overall.p50_ms)}</td>
                <td className="r">{sp(v.overall.p95_ms)}</td>
                <td className="r">{sp(v.overall.p99_ms)}</td>
                {paths.map((p) => (
                  <td className="r" key={p}>
                    {v.paths[p] ? `${fmtMs(v.paths[p].p95_ms.median)} / ${fmtMs(v.paths[p].p99_ms.median)}` : "—"}
                  </td>
                ))}
                <td className="r">{fmtNum(v.errors.median + v.shed.median)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

// ---- theory --------------------------------------------------------------------

function erlangC(c: number, a: number) {
  const rho = a / c;
  if (rho >= 1) return 1;
  let term = 1;
  let sum = 1;
  for (let k = 1; k < c; k++) {
    term *= a / k;
    sum += term;
  }
  const top = ((term * a) / c) / (1 - rho);
  return top / (sum + top);
}

function TheoryResults({ pts, pal, durationS }: { pts: TheoryPoint[]; pal: Palette; durationS: number }) {
  const c = pts[0].servers;
  const service = pts.reduce((s, p) => s + p.service_ms, 0) / pts.length;
  const cv2 = pts.reduce((s, p) => s + p.service_cv2, 0) / pts.length;
  const top = Math.min(0.97, Math.max(...pts.map((p) => p.rho)) + 0.03);
  const curve = Array.from({ length: 60 }, (_, i) => {
    const rho = 0.05 + ((top - 0.05) * i) / 59;
    const a = rho * c;
    const mmc = (erlangC(c, a) * service) / (c - a);
    return { rho, mmc, mgc: (mmc * (1 + cv2)) / 2 };
  });
  const measured = pts.map((p) => ({ rho: p.rho, wq: p.wq_ms }));
  const colors = { mmc: pal.categorical[1], mgc: pal.categorical[0], measured: pal.text };

  return (
    <>
      <div className="chart-grid">
        <section className="card chart-card wide">
          <header className="card-head">
            <div>
              <h3>Mean wait in the queue vs utilization</h3>
              <p className="note">
                {c} servers (CPU cores), measured mean job time {fmtMs(service)}, job-time variability Cs² = {cv2.toFixed(3)}. Dots are measurements.
              </p>
            </div>
            <Legend
              items={[
                { name: "Measured", color: colors.measured },
                { name: "M/G/c prediction", color: colors.mgc },
                { name: "M/M/c prediction", color: colors.mmc },
              ]}
            />
          </header>
          <div style={{ height: 320 }}>
            <ResponsiveContainer>
              <ComposedChart margin={{ top: 8, right: 16, left: 0, bottom: 16 }}>
                <CartesianGrid vertical={false} stroke={pal.grid} />
                <XAxis
                  dataKey="rho"
                  type="number"
                  domain={[0, 1]}
                  ticks={[0, 0.2, 0.4, 0.6, 0.8, 1]}
                  tickFormatter={pct}
                  stroke={pal.axis}
                  tick={{ fill: pal.muted, fontSize: 11 }}
                  tickLine={false}
                  label={{ value: "Utilization", position: "insideBottomRight", offset: -8, fill: pal.muted, fontSize: 11 }}
                />
                <YAxis width={56} tickFormatter={fmtMs} stroke={pal.axis} tick={{ fill: pal.muted, fontSize: 11 }} tickLine={false} axisLine={false} />
                <Tooltip content={<TheoryTip pts={pts} />} isAnimationActive={false} />
                <Line data={curve} dataKey="mmc" name="M/M/c" stroke={colors.mmc} strokeWidth={2} dot={false} isAnimationActive={false} />
                <Line data={curve} dataKey="mgc" name="M/G/c" stroke={colors.mgc} strokeWidth={2} dot={false} isAnimationActive={false} />
                <Scatter
                  data={measured}
                  dataKey="wq"
                  name="Measured"
                  isAnimationActive={false}
                  shape={(p: { cx?: number; cy?: number }) =>
                    p.cx == null || p.cy == null ? <g /> : <circle cx={p.cx} cy={p.cy} r={5} fill={colors.measured} stroke={pal.surface} strokeWidth={2} />
                  }
                />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
        </section>
      </div>
      <TheoryReading pts={pts} cv2={cv2} durationS={durationS} />
      <section className="card">
        <header className="card-head">
          <div>
            <h3>All numbers</h3>
            <p className="note">Wq = mean time waiting in the queue. P(wait) = share of requests that had to queue.</p>
          </div>
        </header>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th className="r">Target ρ</th>
                <th className="r">Measured ρ</th>
                <th className="r">Arrivals/s</th>
                <th className="r">Job time</th>
                <th className="r">Measured Wq</th>
                <th className="r">M/G/c Wq</th>
                <th className="r">M/M/c Wq</th>
                <th className="r">Gap vs M/G/c</th>
                <th className="r">P(wait) measured / Erlang C</th>
              </tr>
            </thead>
            <tbody>
              {pts.map((p) => (
                <tr key={p.target_rho}>
                  <td className="r">{pct(p.target_rho)}</td>
                  <td className="r">{pct(p.rho)}</td>
                  <td className="r">{p.rate.toFixed(1)}</td>
                  <td className="r">{fmtMs(p.service_ms)}</td>
                  <td className="r">{p.wq_ms.toFixed(1)} ms</td>
                  <td className="r">{p.mgc_wq_ms.toFixed(1)} ms</td>
                  <td className="r">{p.mmc_wq_ms.toFixed(1)} ms</td>
                  <td className="r">{p.mgc_wq_ms >= 5 ? `${p.error_pct > 0 ? "+" : ""}${p.error_pct.toFixed(0)}%` : "noise (< 5 ms)"}</td>
                  <td className="r">
                    {pct(p.p_wait)} / {pct(p.erlang_c)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}

function TheoryTip({ active, payload, pts }: { active?: boolean; payload?: { payload: { rho: number } }[]; pts: TheoryPoint[] }) {
  if (!active || !payload?.length) return null;
  const rho = payload[0].payload.rho;
  const p = pts.reduce((best, x) => (Math.abs(x.rho - rho) < Math.abs(best.rho - rho) ? x : best), pts[0]);
  if (Math.abs(p.rho - rho) > 0.02) return null;
  return (
    <div className="tip">
      <div className="tip-time">Utilization {pct(p.rho)}</div>
      <div className="tip-row"><span>Measured wait</span><b>{p.wq_ms.toFixed(1)} ms</b></div>
      <div className="tip-row"><span>M/G/c</span><b>{p.mgc_wq_ms.toFixed(1)} ms</b></div>
      <div className="tip-row"><span>M/M/c</span><b>{p.mmc_wq_ms.toFixed(1)} ms</b></div>
      <div className="tip-row"><span>Requests</span><b>{fmtNum(p.requests)}</b></div>
    </div>
  );
}

function TheoryReading({ pts, cv2, durationS }: { pts: TheoryPoint[]; cv2: number; durationS: number }) {
  // Under 5 ms of predicted wait, timer and network noise is as large as the effect.
  const measurable = pts.filter((p) => p.mgc_wq_ms >= 5);
  const within = measurable.filter((p) => Math.abs(p.error_pct) <= 10).length;
  return (
    <section className="card exp-reading">
      <h3>Reading it</h3>
      <ul>
        <li>
          <b>Why two predictions.</b> M/M/c assumes job times vary randomly (exponentially). Here every /search takes almost
          exactly the same time (Cs² = {cv2.toFixed(3)}), and constant job times roughly halve the queueing. The M/G/c
          line applies that correction (Allen–Cunneen: × (1 + Cs²) / 2), so it is the fair comparison.
        </li>
        <li>
          <b>Where they match.</b> {within} of {measurable.length} steps with a predicted wait of 5 ms or more land within
          10% of M/G/c, and the measured share of requests that had to queue tracks Erlang C closely.
        </li>
        <li>
          <b>Where they diverge.</b> At low utilization the predicted waits are under a millisecond, so timer and network
          noise dominate. Near 95% the waits depend on rare long bursts, so a {durationS} s window is a small sample and
          runs scatter. The Allen–Cunneen formula is also an approximation, best at high load.
        </li>
      </ul>
    </section>
  );
}

// ---- downloads + history ---------------------------------------------------------

function download(name: string, text: string, type: string) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

function toCSV(e: Experiment) {
  if (e.spec.kind === "theory") {
    const head = "target_rho,rho,rate,servers,service_ms,service_cv2,requests,wq_ms,mgc_wq_ms,mmc_wq_ms,error_pct,p_wait,erlang_c";
    return [head, ...(e.theory ?? []).map((p) =>
      [p.target_rho, p.rho, p.rate, p.servers, p.service_ms, p.service_cv2, p.requests, p.wq_ms, p.mgc_wq_ms, p.mmc_wq_ms, p.error_pct, p.p_wait, p.erlang_c].join(","),
    )].join("\n");
  }
  const head = "variant,repeat,seed,throughput,errors,shed,p50_ms,p95_ms,p99_ms,max_ms";
  return [head, ...(e.runs ?? []).map((r) =>
    [JSON.stringify(r.variant), r.repeat, r.seed, r.throughput, r.errors, r.shed, r.overall.p50_ms, r.overall.p95_ms, r.overall.p99_ms, r.overall.max_ms].join(","),
  )].join("\n");
}

function Downloads({ e }: { e: Experiment }) {
  if (e.status !== "done") return null;
  return (
    <div className="exp-downloads">
      <button className="btn sm" onClick={() => download(`${e.id}.json`, JSON.stringify(e, null, 2), "application/json")}>JSON</button>
      <button className="btn sm" onClick={() => download(`${e.id}.csv`, toCSV(e), "text/csv")}>CSV</button>
    </div>
  );
}

function History({ list, selected, onSelect }: { list: Experiment[]; selected: string | null; onSelect: (id: string) => void }) {
  if (!list.length) return null;
  return (
    <section className="card">
      <header className="card-head">
        <h3>Past experiments on this server</h3>
      </header>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Started</th>
              <th>Kind</th>
              <th>Status</th>
              <th>Finding</th>
            </tr>
          </thead>
          <tbody>
            {list.map((e) => (
              <tr key={e.id} className={e.id === selected ? "row-on" : ""}>
                <td>
                  <button className="linkish" onClick={() => onSelect(e.id)}>{new Date(e.started).toLocaleString()}</button>
                </td>
                <td>{e.spec.kind === "theory" ? "Theory" : `Compare (${e.spec.variants?.length ?? 0})`}</td>
                <td>{e.status}</td>
                <td className="wrap">{e.finding || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
