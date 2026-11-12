import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceLine,
  ResponsiveContainer,
  Scatter,
  ScatterChart,
  Tooltip,
  XAxis,
  YAxis,
  ZAxis,
} from "recharts";
import type { LBEvent } from "../live";
import { totals } from "../live";
import { fmtClock, fmtMs, fmtNum, type Palette } from "../theme";
import { algoLabel, type Point } from "../types";

export interface Series {
  key: string;
  name: string;
  color: string;
  dashed?: boolean; // a second encoding when two series share a colour
  get: (p: Point) => number | null;
}

interface TimeChartProps {
  title: string;
  note?: string;
  points: Point[];
  series: Series[];
  events: LBEvent[];
  pal: Palette;
  fmt: (v: number) => string;
  height?: number;
}

type Row = { t: number } & Record<string, number | null>;

/** A time-series line chart with crosshair tooltip and change markers. */
export function TimeChart({ title, note, points, series, events, pal, fmt, height = 190 }: TimeChartProps) {
  const rows: Row[] = points.map((p) => {
    const r: Row = { t: p.t };
    for (const s of series) r[s.key] = s.get(p);
    return r;
  });
  const last = rows.length - 1;
  const multi = series.length > 1;
  const endLabels = multi && series.length <= 4; // more than 4 end labels collide; the legend carries them
  const nudge = endLabels ? labelNudges(rows, series, height - 38) : {};

  return (
    <section className="card chart-card">
      <header className="card-head">
        <div>
          <h3>{title}</h3>
          {note && <p className="note">{note}</p>}
        </div>
        {multi && <Legend items={series.map((s) => ({ name: s.name, color: s.color, dashed: s.dashed }))} />}
      </header>
      {rows.length < 2 ? (
        <Empty height={height} />
      ) : (
        <div style={{ height }}>
          <ResponsiveContainer>
            <LineChart data={rows} margin={{ top: 8, right: endLabels ? 64 : 16, left: 0, bottom: 0 }}>
              <CartesianGrid vertical={false} stroke={pal.grid} />
              <XAxis
                dataKey="t"
                type="number"
                scale="time"
                domain={["dataMin", "dataMax"]}
                tickFormatter={fmtClock}
                stroke={pal.axis}
                tick={{ fill: pal.muted, fontSize: 11 }}
                tickLine={false}
                minTickGap={48}
              />
              <YAxis
                width={52}
                tickFormatter={fmt}
                stroke={pal.axis}
                tick={{ fill: pal.muted, fontSize: 11 }}
                tickLine={false}
                axisLine={false}
              />
              <Tooltip
                content={<TimeTip events={events} fmt={fmt} />}
                cursor={{ stroke: pal.axis, strokeWidth: 1 }}
                isAnimationActive={false}
              />
              {events.map((e) => (
                <ReferenceLine key={e.t} x={e.t} stroke={pal.muted} strokeWidth={1} strokeOpacity={0.6} />
              ))}
              {series.map((s) => (
                <Line
                  key={s.key}
                  dataKey={s.key}
                  name={s.name}
                  stroke={s.color}
                  strokeWidth={2}
                  strokeDasharray={s.dashed ? "5 4" : undefined}
                  dot={false}
                  activeDot={{ r: 4, strokeWidth: 2, stroke: pal.surface }}
                  connectNulls
                  isAnimationActive={false}
                  label={
                    endLabels
                      ? (props: { index?: number; x?: number; y?: number }) =>
                          props.index === last && props.x != null && props.y != null ? (
                            <text x={props.x + 6} y={props.y} dy={4 + (nudge[s.key] ?? 0)} fill={pal.text2} fontSize={11}>
                              {s.name}
                            </text>
                          ) : (
                            <g />
                          )
                      : undefined
                  }
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </div>
      )}
    </section>
  );
}

/** Vertical pixel offsets so end-of-line labels never overlap. Uses the
 * series' last values against an approximate 0..max scale (close enough:
 * Recharts rounds the max up to a nice tick). */
function labelNudges(rows: Row[], series: Series[], plotH: number): Record<string, number> {
  const GAP = 13;
  let max = 0;
  for (const r of rows) for (const s of series) max = Math.max(max, r[s.key] ?? 0);
  if (max <= 0 || rows.length === 0) return {};
  const lastRow = rows[rows.length - 1];
  const ends = series
    .map((s) => ({ key: s.key, y: (1 - (lastRow[s.key] ?? 0) / max) * plotH }))
    .sort((a, b) => a.y - b.y);
  const placed: number[] = [];
  let prev = -Infinity;
  for (const e of ends) {
    prev = Math.max(e.y, prev + GAP);
    placed.push(prev);
  }
  // Labels pushed past the bottom of the plot would sit on the time axis:
  // shift the whole stack up instead.
  const overflow = Math.max(0, (placed.at(-1) ?? 0) - plotH);
  const out: Record<string, number> = {};
  ends.forEach((e, i) => {
    out[e.key] = placed[i] - overflow - e.y;
  });
  return out;
}

interface TipProps {
  active?: boolean;
  label?: number;
  payload?: { dataKey: string; name: string; value: number | null; color: string }[];
  events: LBEvent[];
  fmt: (v: number) => string;
}

function TimeTip({ active, label, payload, events, fmt }: TipProps) {
  if (!active || !payload?.length || label == null) return null;
  const here = events.filter((e) => Math.abs(e.t - label) < 500);
  return (
    <div className="tip">
      <div className="tip-time">{fmtClock(label)}</div>
      {payload.map((p) => (
        <div className="tip-row" key={p.dataKey}>
          <span className="swatch" style={{ background: p.color }} />
          <span>{p.name}</span>
          <b>{p.value == null ? "—" : fmt(p.value)}</b>
        </div>
      ))}
      {here.map((e) => (
        <div className="tip-event" key={e.t}>
          ▸ {e.label}
        </div>
      ))}
    </div>
  );
}

export function Legend({ items }: { items: { name: string; color: string; dashed?: boolean }[] }) {
  return (
    <ul className="legend">
      {items.map((i) => (
        <li key={i.name}>
          {i.dashed ? (
            <span className="swatch-line dashed" style={{ borderColor: i.color }} />
          ) : (
            <span className="swatch" style={{ background: i.color }} />
          )}
          {i.name}
        </li>
      ))}
    </ul>
  );
}

function Empty({ height }: { height: number }) {
  return (
    <div className="empty" style={{ height }}>
      Waiting for data. Add some users to generate traffic.
    </div>
  );
}

// ---- load curve -------------------------------------------------------------

interface CurveRow {
  users: number;
  p95: number;
  cfg: string;
  t: number;
}

/** p95 latency against concurrent users. The current configuration is
 * highlighted; earlier configurations stay in gray for comparison. */
export function LoadCurve({ points, pal }: { points: Point[]; pal: Palette }) {
  const cfgOf = (p: Point) => {
    const t = totals(p);
    return `${t.cores} cores · ${t.db} DB · ${algoLabel(p.algorithm)}`;
  };
  const busy = points.filter((p) => p.users > 0 && p.requests > 0);
  const current = busy.length ? cfgOf(busy[busy.length - 1]) : "";
  const rows: CurveRow[] = busy.map((p) => ({ users: p.users, p95: p.p95_ms, cfg: cfgOf(p), t: p.t }));
  const now = rows.filter((r) => r.cfg === current);
  const before = rows.filter((r) => r.cfg !== current);
  const height = 240;

  const dot =
    (fill: string) =>
    (props: { cx?: number; cy?: number }) =>
      props.cx == null || props.cy == null ? (
        <g />
      ) : (
        <circle cx={props.cx} cy={props.cy} r={4} fill={fill} stroke={pal.surface} strokeWidth={2} />
      );

  return (
    <section className="card chart-card wide">
      <header className="card-head">
        <div>
          <h3>Load curve: p95 latency vs. users</h3>
          <p className="note">
            One dot per second. Change users or resources and compare the curves.
          </p>
        </div>
        <Legend
          items={[
            { name: current ? `Now: ${current}` : "Now", color: pal.p95 },
            { name: "Earlier configurations", color: pal.dim },
          ]}
        />
      </header>
      {rows.length < 2 ? (
        <Empty height={height} />
      ) : (
        <div style={{ height }}>
          <ResponsiveContainer>
            <ScatterChart margin={{ top: 8, right: 16, left: 0, bottom: 8 }}>
              <CartesianGrid vertical={false} stroke={pal.grid} />
              <XAxis
                dataKey="users"
                type="number"
                name="Users"
                stroke={pal.axis}
                tick={{ fill: pal.muted, fontSize: 11 }}
                tickLine={false}
                label={{ value: "Concurrent users", position: "insideBottomRight", offset: -4, fill: pal.muted, fontSize: 11 }}
              />
              <YAxis
                dataKey="p95"
                type="number"
                width={52}
                tickFormatter={fmtMs}
                stroke={pal.axis}
                tick={{ fill: pal.muted, fontSize: 11 }}
                tickLine={false}
                axisLine={false}
              />
              <ZAxis range={[64, 64]} />
              <Tooltip content={<CurveTip />} cursor={false} isAnimationActive={false} />
              <Scatter data={before} shape={dot(pal.dim)} isAnimationActive={false} />
              <Scatter data={now} shape={dot(pal.p95)} isAnimationActive={false} />
            </ScatterChart>
          </ResponsiveContainer>
        </div>
      )}
    </section>
  );
}

function CurveTip({ active, payload }: { active?: boolean; payload?: { payload: CurveRow }[] }) {
  if (!active || !payload?.length) return null;
  const r = payload[0].payload;
  return (
    <div className="tip">
      <div className="tip-time">{fmtClock(r.t)}</div>
      <div className="tip-row">
        <span>Users</span>
        <b>{fmtNum(r.users)}</b>
      </div>
      <div className="tip-row">
        <span>p95 latency</span>
        <b>{fmtMs(r.p95)}</b>
      </div>
      <div className="tip-event">{r.cfg}</div>
    </div>
  );
}

// ---- sparkline for backend cards -------------------------------------------

export function Sparkline({
  points,
  name,
  pal,
}: {
  points: Point[];
  name: string;
  pal: Palette;
}) {
  const rows = points.map((p) => {
    const b = p.backends?.find((x) => x.name === name);
    return { t: p.t, inflight: b ? b.inflight : null, capacity: b ? b.capacity : null };
  });
  const cap = rows.length ? rows[rows.length - 1].capacity : null;
  if (rows.length < 2) return <div className="spark-empty" />;
  return (
    <div className="spark" aria-hidden>
      <ResponsiveContainer>
        <LineChart data={rows} margin={{ top: 4, right: 2, left: 2, bottom: 2 }}>
          <YAxis hide domain={[0, (max: number) => Math.max(max, cap ?? 0)]} />
          <XAxis dataKey="t" hide type="number" domain={["dataMin", "dataMax"]} />
          {cap != null && <ReferenceLine y={cap} stroke={pal.muted} strokeWidth={1} strokeOpacity={0.7} />}
          <Tooltip
            content={({ active, payload, label }) =>
              active && payload?.length ? (
                <div className="tip">
                  <div className="tip-time">{fmtClock(label as number)}</div>
                  <div className="tip-row">
                    <span>In flight</span>
                    <b>{payload[0].value as number}</b>
                  </div>
                </div>
              ) : null
            }
            cursor={{ stroke: pal.axis, strokeWidth: 1 }}
            isAnimationActive={false}
          />
          <Line dataKey="inflight" stroke={pal.p95} strokeWidth={2} dot={false} isAnimationActive={false} />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
