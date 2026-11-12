// Mirrors the JSON from backend/internal/lb (admin.go, balancer.go, metrics.go).

export type Algorithm = "round_robin" | "least_conn" | "fcfs" | "sjf" | "auto";

export const ALGORITHMS: { id: Algorithm; label: string; hint: string }[] = [
  { id: "auto", label: "Auto", hint: "Round robin at low load; SJF or FCFS when backends are saturated" },
  { id: "round_robin", label: "Round robin", hint: "Send each request to the next backend in turn, no LB queue" },
  { id: "least_conn", label: "Least conn", hint: "Send each request to the backend with the fewest in flight (relative to its slots), no LB queue" },
  { id: "fcfs", label: "FCFS", hint: "Queue at the LB, oldest request first, to the least-loaded backend" },
  { id: "sjf", label: "SJF", hint: "Queue at the LB, shortest expected job first (with aging)" },
];

export const algoLabel = (a: string) => ALGORITHMS.find((x) => x.id === a)?.label ?? a;

export interface BackendView {
  name: string;
  url: string;
  healthy: boolean;
  inflight: number;
  capacity: number;
  max_inflight: number;
  served: number;
  proxy_errors: number;
  cores: number;
  db_pool: number;
  cpu_busy: number;
  cpu_waiting: number;
  db_busy: number;
  db_waiting: number;
  draining: boolean;
  ejected: boolean;
  ejected_for_s: number;
  ejections: number;
  fault_for_s: number;
}

export interface LoadConfig {
  users: number;
  think_ms: number;
  mix: Record<string, number>;
}

export interface State {
  algorithm: Algorithm;
  effective: Algorithm;
  reason: string;
  switches: number;
  aging: number;
  queue_len: number;
  queue_by_path: Record<string, number>;
  max_queue: number;
  queue_timeout_ms: number;
  utilization: number;
  backends: BackendView[];
  load: LoadConfig;
  estimates: Record<string, { est_ms: number; samples: number }>;
  limits?: Limits;
  experiment?: Experiment; // the running one, if any
}

/** Server-side caps (public demo mode). 0 = no extra cap. */
export interface Limits {
  max_users: number;
  max_cores: number;
  max_db_pool: number;
  lock_backends: boolean;
  idle_stop_s: number;
  max_fault_s: number;
  max_experiment_s: number;
}

export interface BackendPoint {
  name: string;
  healthy: boolean;
  ejected: boolean;
  draining: boolean;
  down: boolean;
  requests: number;
  inflight: number;
  capacity: number;
  cores: number;
  db_pool: number;
  cpu_busy: number;
  cpu_waiting: number;
  db_busy: number;
  db_waiting: number;
}

export interface PathPoint {
  requests: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  est_ms: number;
  work_ms: number;
  queued: number;
}

export interface Point {
  t: number;
  rps: number;
  requests: number;
  errors: number;
  shed: number;
  rate_limited: number;
  retries: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  avg_ms: number;
  queue_wait_p95_ms: number;
  queue_len: number;
  utilization: number;
  algorithm: Algorithm;
  effective: Algorithm;
  aging?: number;
  users: number;
  backends: BackendPoint[] | null;
  paths: Record<string, PathPoint> | null;
}

// ---- experiments (backend/internal/lb/experiment.go) -----------------------

export interface Variant {
  name: string;
  algorithm: Algorithm;
  aging?: number;
}

export interface ExperimentSpec {
  kind: "compare" | "theory";
  variants?: Variant[];
  users?: number;
  think_ms?: number;
  mix?: Record<string, number>;
  seed?: number;
  duration_s: number;
  warmup_s: number;
  repeats: number;
  path?: string;
  utilizations?: number[];
}

export interface Spread {
  median: number;
  min: number;
  max: number;
}

export interface PathSummary {
  p50_ms: Spread;
  p95_ms: Spread;
  p99_ms: Spread;
  max_ms: Spread;
  max_wait_ms: Spread;
}

export interface VariantSummary {
  variant: string;
  algorithm: Algorithm;
  runs: number;
  throughput: Spread;
  errors: Spread;
  shed: Spread;
  overall: PathSummary;
  paths: Record<string, PathSummary>;
}

export interface TheoryPoint {
  target_rho: number;
  rho: number;
  rate: number;
  servers: number;
  service_ms: number;
  service_cv2: number;
  requests: number;
  wq_ms: number;
  resp_ms: number;
  p_wait: number;
  erlang_c: number;
  mmc_wq_ms: number;
  mgc_wq_ms: number;
  error_pct: number;
  dropped_open: number;
}

export interface RunResult {
  variant: string;
  repeat: number;
  seed: number;
  throughput: number;
  errors: number;
  shed: number;
  overall: { p50_ms: number; p95_ms: number; p99_ms: number; max_ms: number };
}

export interface Experiment {
  id: string;
  spec: ExperimentSpec;
  status: "running" | "done" | "cancelled" | "failed";
  error?: string;
  started: string;
  finished?: string;
  step: number;
  steps: number;
  current: string;
  eta_s: number;
  runs?: RunResult[];
  summary?: VariantSummary[];
  theory?: TheoryPoint[];
  finding?: string;
}
