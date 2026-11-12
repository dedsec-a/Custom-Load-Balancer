import { useState } from "react";
import { api } from "../api";
import type { Palette } from "../theme";
import type { BackendView, Point } from "../types";
import { Sparkline } from "./Charts";

interface Props {
  locked?: boolean;
  maxFault?: number;
  backends: BackendView[];
  points: Point[];
  pal: Palette;
  onError: (msg: string) => void;
  onChanged: () => void;
}

export function Backends({ locked, maxFault = 120, backends, points, pal, onError, onChanged }: Props) {
  return (
    <section className="card">
      <header className="card-head">
        <div>
          <h3>Backends</h3>
          <p className="note">The line shows requests in flight; the gray rule is the LB's slot limit (cores + DB pool).</p>
        </div>
      </header>
      <div className="backend-grid">
        {backends.map((b) => (
          <BackendCard key={b.name} b={b} locked={locked} maxFault={maxFault} points={points} pal={pal} onError={onError} onChanged={onChanged} />
        ))}
        {!locked && <AddBackend onError={onError} onChanged={onChanged} next={backends.length + 1} />}
      </div>
    </section>
  );
}

function Meter({ label, used, size, waiting }: { label: string; used: number; size: number; waiting?: number }) {
  const pct = size > 0 ? Math.min(100, (used / size) * 100) : 0;
  return (
    <div className="meter">
      <div className="meter-row">
        <span>{label}</span>
        <span className="meter-val">
          {used}/{size}
          {waiting ? <em> · {waiting} waiting</em> : null}
        </span>
      </div>
      <div className="meter-track" role="meter" aria-label={label} aria-valuenow={used} aria-valuemin={0} aria-valuemax={size}>
        <div className="meter-fill" style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

/** What the card says about a backend's health, most specific first. */
function healthOf(b: BackendView): { cls: string; icon: string; text: string; detail?: string } {
  if (b.draining) return { cls: "warn", icon: "◐", text: "Draining", detail: "finishing in-flight requests, then leaving" };
  if (b.fault_for_s > 0)
    return { cls: "bad", icon: "✕", text: "Down", detail: `simulated outage, ${Math.ceil(b.fault_for_s)} s left` };
  if (b.ejected)
    return { cls: "bad", icon: "✕", text: "Ejected", detail: `failed real requests; retrying in ${Math.ceil(b.ejected_for_s)} s` };
  if (!b.healthy) return { cls: "bad", icon: "✕", text: "Down", detail: "failing health checks" };
  return { cls: "good", icon: "●", text: "Healthy" };
}

function BackendCard({ b, locked, maxFault, points, pal, onError, onChanged }: { b: BackendView; maxFault: number } & Omit<Props, "backends" | "maxFault">) {
  const [cores, setCores] = useState(String(b.cores));
  const [db, setDb] = useState(String(b.db_pool));
  const [editing, setEditing] = useState(false);

  const apply = () => {
    const c = Number(cores), d = Number(db);
    api
      .setBackendResources(b.name, { cores: c, db_pool: d })
      .then(() => {
        setEditing(false);
        onChanged();
      })
      .catch((e: Error) => onError(e.message));
  };

  const h = healthOf(b);
  const outageFor = Math.min(60, maxFault);
  const fault = (secs: number) =>
    api.setFault(b.name, secs).then(onChanged).catch((e: Error) => onError(e.message));

  return (
    <article className={`backend ${h.cls === "good" ? "" : "is-" + h.cls}`}>
      <header className="backend-head">
        <b>{b.name}</b>
        <span className={`status ${h.cls}`}>
          <span aria-hidden>{h.icon}</span> {h.text}
        </span>
      </header>
      <div className="backend-url">{h.detail ?? b.url}</div>
      <Sparkline points={points} name={b.name} pal={pal} />
      <Meter label="LB slots" used={b.inflight} size={b.capacity} />
      <Meter label="CPU cores" used={b.cpu_busy} size={b.cores} waiting={b.cpu_waiting} />
      <Meter label="DB connections" used={b.db_busy} size={b.db_pool} waiting={b.db_waiting} />
      <div className="backend-fault">
        {b.fault_for_s > 0 ? (
          <button className="btn sm" onClick={() => fault(0)}>Recover now</button>
        ) : (
          <button className="btn sm danger" onClick={() => fault(outageFor)} title={`The backend answers 503 for ${outageFor} s. Watch the LB eject it and retry elsewhere.`}>
            Simulate outage ({outageFor} s)
          </button>
        )}
        <span className="muted">{b.ejections > 0 ? `ejected ${b.ejections}×` : ""}</span>
      </div>
      <div className="backend-foot">
        <span className="muted">{b.served.toLocaleString()} served</span>
        {editing ? (
          <span className="edit">
            <input aria-label="Cores" className="num sm" type="number" min={1} max={256} value={cores} onChange={(e) => setCores(e.target.value)} />
            <span className="muted">cores</span>
            <input aria-label="DB pool" className="num sm" type="number" min={1} max={1000} value={db} onChange={(e) => setDb(e.target.value)} />
            <span className="muted">DB</span>
            <button className="btn primary sm" onClick={apply}>Apply</button>
          </span>
        ) : (
          <span className="edit">
            <button
              className="btn sm"
              onClick={() => {
                setCores(String(b.cores));
                setDb(String(b.db_pool));
                setEditing(true);
              }}
            >
              Edit
            </button>
            {!locked && (
            <button
              className="btn sm danger"
              onClick={() => {
                if (confirm(`Stop sending traffic to ${b.name}?`)) {
                  api.removeBackend(b.name).then(onChanged).catch((e: Error) => onError(e.message));
                }
              }}
            >
              Remove
            </button>
            )}
          </span>
        )}
      </div>
    </article>
  );
}

function AddBackend({ onError, onChanged, next }: { onError: (m: string) => void; onChanged: () => void; next: number }) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  return (
    <form
      className="backend add"
      onSubmit={(e) => {
        e.preventDefault();
        const n = name || `b${next}`;
        const u = url || `http://127.0.0.1:${3000 + next}`;
        api
          .addBackend(n, u)
          .then(() => {
            setName("");
            setUrl("");
            onChanged();
          })
          .catch((err: Error) => onError(err.message));
      }}
    >
      <b>Add a backend</b>
      <p className="hint">Start it first, e.g. <code>bin\backend.exe --port {3000 + next} --name b{next}</code></p>
      <input placeholder={`b${next}`} value={name} onChange={(e) => setName(e.target.value)} aria-label="Name" />
      <input placeholder={`http://127.0.0.1:${3000 + next}`} value={url} onChange={(e) => setUrl(e.target.value)} aria-label="URL" />
      <button className="btn primary" type="submit">Add</button>
    </form>
  );
}
