import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import { ALGORITHMS, algoLabel, type Algorithm, type State } from "../types";

/** Local slider value that follows the server unless the user just touched it. */
function useSynced<T>(server: T | undefined, fallback: T): [T, (v: T) => void] {
  const [v, setV] = useState<T>(server ?? fallback);
  const touched = useRef(0);
  const sig = JSON.stringify(server);
  useEffect(() => {
    if (server !== undefined && Date.now() - touched.current > 1500) setV(server);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sig]);
  return [
    v,
    (x: T) => {
      touched.current = Date.now();
      setV(x);
    },
  ];
}

/** Sends the latest value once the user pauses (sliders fire on every pixel). */
function useDebounced<T>(fn: (v: T) => void, ms = 250) {
  const timer = useRef<number | undefined>(undefined);
  const latest = useRef(fn);
  latest.current = fn;
  return (v: T) => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => latest.current(v), ms);
  };
}

interface Props {
  state: State | null;
  onError: (msg: string) => void;
  onChanged: () => void;
}

const wrap = (p: Promise<unknown>, onError: (m: string) => void, onChanged: () => void) =>
  p.then(onChanged).catch((e: Error) => onError(e.message));

// ---- traffic ----------------------------------------------------------------

const USER_PRESETS = [0, 10, 50, 150, 500];
const cap = (limit: number | undefined, fallback: number) => (limit && limit > 0 ? Math.min(limit, fallback) : fallback);

export function TrafficPanel({ state, onError, onChanged }: Props) {
  const [users, setUsers] = useSynced(state?.load.users, 0);
  const [think, setThink] = useSynced(state?.load.think_ms, 0);
  const [mix, setMix] = useSynced(state?.load.mix, {} as Record<string, number>);
  const send = useDebounced((b: Parameters<typeof api.setLoad>[0]) => wrap(api.setLoad(b), onError, onChanged));
  const maxUsers = cap(state?.limits?.max_users, 2000);
  const sliderMax = Math.min(maxUsers, 1000);
  const presets = USER_PRESETS.filter((n) => n <= maxUsers);
  const idle = state?.limits?.idle_stop_s ?? 0;

  const total = Object.values(mix).reduce((s, w) => s + w, 0) || 1;
  const setUsersNow = (n: number) => {
    const v = Math.max(0, Math.min(maxUsers, Math.round(n || 0)));
    setUsers(v);
    send({ users: v });
  };

  return (
    <section className="panel">
      <h2>Traffic</h2>
      <div className="field">
        <div className="field-row">
          <label htmlFor="users">Virtual users</label>
          <input
            className="num"
            type="number"
            min={0}
            max={maxUsers}
            value={users}
            onChange={(e) => setUsersNow(Number(e.target.value))}
            aria-label="Virtual users (number)"
          />
        </div>
        <input
          id="users"
          type="range"
          min={0}
          max={sliderMax}
          step={5}
          value={Math.min(users, sliderMax)}
          onChange={(e) => setUsersNow(Number(e.target.value))}
        />
        <div className="chips">
          {presets.map((n) => (
            <button key={n} className={users === n ? "chip on" : "chip"} onClick={() => setUsersNow(n)}>
              {n === 0 ? "Stop" : n}
            </button>
          ))}
        </div>
        <p className="hint">
          Each user sends a request, waits for the reply, then sends the next one.
          {idle > 0 && ` On this server users stop after ${Math.round(idle / 60)} min without a change.`}
        </p>
      </div>

      <div className="field">
        <div className="field-row">
          <label htmlFor="think">Think time</label>
          <output>{think} ms</output>
        </div>
        <input
          id="think"
          type="range"
          min={0}
          max={1000}
          step={10}
          value={think}
          onChange={(e) => {
            const v = Number(e.target.value);
            setThink(v);
            send({ think_ms: v });
          }}
        />
      </div>

      <div className="field">
        <span className="label">Request mix</span>
        {Object.keys(mix)
          .sort()
          .map((path) => (
            <div key={path} className="mix-row">
              <code>{path}</code>
              <input
                type="range"
                min={0}
                max={100}
                value={mix[path]}
                aria-label={`Weight for ${path}`}
                onChange={(e) => {
                  const next = { ...mix, [path]: Number(e.target.value) };
                  if (Object.values(next).every((w) => w === 0)) return; // the LB rejects an all-zero mix
                  setMix(next);
                  send({ mix: next });
                }}
              />
              <output>{Math.round((mix[path] / total) * 100)}%</output>
            </div>
          ))}
      </div>
    </section>
  );
}

// ---- resources --------------------------------------------------------------

export function ResourcesPanel({ state, onError, onChanged }: Props) {
  const first = state?.backends[0];
  const mixed =
    !!state && state.backends.some((b) => b.cores !== first?.cores || b.db_pool !== first?.db_pool);
  const [cores, setCores] = useSynced(first?.cores, 2);
  const [db, setDb] = useSynced(first?.db_pool, 5);
  const maxCores = cap(state?.limits?.max_cores, 32);
  const maxDB = cap(state?.limits?.max_db_pool, 100);
  const send = useDebounced((b: Parameters<typeof api.setResources>[0]) =>
    wrap(api.setResources(b), onError, onChanged),
  );

  return (
    <section className="panel">
      <h2>Resources (every backend)</h2>
      <div className="field">
        <div className="field-row">
          <label htmlFor="cores">CPU cores</label>
          <output>{cores}</output>
        </div>
        <input
          id="cores"
          type="range"
          min={1}
          max={maxCores}
          value={cores}
          onChange={(e) => {
            const v = Number(e.target.value);
            setCores(v);
            send({ cores: v });
          }}
        />
      </div>
      <div className="field">
        <div className="field-row">
          <label htmlFor="db">DB connection pool</label>
          <output>{db}</output>
        </div>
        <input
          id="db"
          type="range"
          min={1}
          max={maxDB}
          value={db}
          onChange={(e) => {
            const v = Number(e.target.value);
            setDb(v);
            send({ db_pool: v });
          }}
        />
      </div>
      <p className="hint">
        {mixed ? "Backends differ right now; moving a slider sets all of them. " : ""}
        Adding capacity takes effect at once. Removing it takes effect as running requests finish.
      </p>
    </section>
  );
}

// ---- algorithm --------------------------------------------------------------

export function AlgorithmPanel({ state, onError, onChanged }: Props) {
  const [aging, setAging] = useSynced(state?.aging, 0.02);
  const [maxQueue, setMaxQueue] = useSynced(state?.max_queue, 10000);
  const [timeoutMs, setTimeoutMs] = useSynced(state?.queue_timeout_ms, 10000);
  const sendQueue = useDebounced((b: { max_queue?: number; queue_timeout_ms?: number }) =>
    wrap(api.setAlgorithm(b), onError, onChanged),
  600);
  const sendAging = useDebounced((v: number) => wrap(api.setAlgorithm({ aging: v }), onError, onChanged));
  const selected = state?.algorithm ?? "auto";
  const pick = (a: Algorithm) => wrap(api.setAlgorithm({ algorithm: a }), onError, onChanged);

  return (
    <section className="panel">
      <h2>Scheduling algorithm</h2>
      <div className="segmented" role="radiogroup" aria-label="Algorithm">
        {ALGORITHMS.map((a) => (
          <button
            key={a.id}
            role="radio"
            aria-checked={selected === a.id}
            className={selected === a.id ? "seg on" : "seg"}
            onClick={() => pick(a.id)}
            title={a.hint}
          >
            {a.label}
          </button>
        ))}
      </div>
      <p className="hint">{ALGORITHMS.find((a) => a.id === selected)?.hint}</p>
      {state && (
        <div className="running">
          <span className="label">Running now</span>
          <b>{algoLabel(state.effective)}</b>
          {state.algorithm === "auto" && <span className="reason">{state.reason}</span>}
        </div>
      )}

      <div className="field">
        <div className="field-row">
          <label htmlFor="aging">SJF aging</label>
          <output>{aging.toFixed(3)}</output>
        </div>
        <input
          id="aging"
          type="range"
          min={0}
          max={0.2}
          step={0.005}
          value={aging}
          onChange={(e) => {
            const v = Number(e.target.value);
            setAging(v);
            sendAging(v);
          }}
        />
        <div className="scale-ends">
          <span>Short jobs first</span>
          <span>Fairer to long jobs</span>
        </div>
        <p className="hint">
          Each second a request waits lowers its job size by aging × 1000 ms, so long jobs can't wait forever.
        </p>
      </div>

      <div className="field">
        <span className="label">Backpressure (FCFS / SJF queue)</span>
        <div className="field-row">
          <label htmlFor="maxq">Max queue</label>
          <input
            id="maxq"
            className="num"
            type="number"
            min={1}
            max={1000000}
            value={maxQueue}
            onChange={(e) => {
              const v = Math.max(1, Math.round(Number(e.target.value) || 1));
              setMaxQueue(v);
              sendQueue({ max_queue: v });
            }}
          />
        </div>
        <div className="field-row">
          <label htmlFor="qto">Queue timeout (ms)</label>
          <input
            id="qto"
            className="num"
            type="number"
            min={1}
            max={300000}
            step={500}
            value={timeoutMs}
            onChange={(e) => {
              const v = Math.max(1, Math.round(Number(e.target.value) || 1));
              setTimeoutMs(v);
              sendQueue({ queue_timeout_ms: v });
            }}
          />
        </div>
        <p className="hint">
          When the queue is full or a request waits too long, it's turned away with 503 (shed) instead of piling up.
          Try max queue 50 at 150 users.
        </p>
      </div>
    </section>
  );
}
