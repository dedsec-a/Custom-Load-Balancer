import type { Algorithm, BackendView, Experiment, ExperimentSpec, LoadConfig, Point, State } from "./types";

// Empty = same origin (Vite dev proxy, or the LB serving the built UI).
// Set VITE_API_BASE=http://host:8081 to talk to a remote LB directly.
const BASE: string = import.meta.env.VITE_API_BASE ?? "";

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data as { error?: string }).error ?? `${res.status} ${res.statusText}`);
  return data as T;
}

export interface ResourcesBody {
  cores?: number;
  db_pool?: number;
  max_inflight?: number;
}

export const api = {
  state: () => call<State>("GET", "/api/state"),
  metrics: (last: number) => call<Point[]>("GET", `/api/metrics?last=${last}`),
  setLoad: (b: Partial<LoadConfig>) => call<LoadConfig>("PUT", "/api/load", b),
  setAlgorithm: (b: { algorithm?: Algorithm; aging?: number; max_queue?: number; queue_timeout_ms?: number }) =>
    call<State>("PUT", "/api/algorithm", b),
  setFault: (name: string, downForS: number) =>
    call<BackendView[]>("PUT", `/api/backends/${encodeURIComponent(name)}/fault`, { down_for_s: downForS }),
  setResources: (b: ResourcesBody) => call<BackendView[]>("PUT", "/api/resources", b),
  setBackendResources: (name: string, b: ResourcesBody) =>
    call<BackendView[]>("PUT", `/api/backends/${encodeURIComponent(name)}/resources`, b),
  addBackend: (name: string, url: string) => call<BackendView[]>("POST", "/api/backends", { name, url }),
  removeBackend: (name: string) => call<BackendView[]>("DELETE", `/api/backends/${encodeURIComponent(name)}`),
  experiments: () => call<Experiment[]>("GET", "/api/experiments"),
  experiment: (id: string) => call<Experiment>("GET", `/api/experiments/${encodeURIComponent(id)}`),
  startExperiment: (spec: ExperimentSpec) => call<Experiment>("POST", "/api/experiments", spec),
  cancelExperiment: (id: string) => call<unknown>("POST", `/api/experiments/${encodeURIComponent(id)}/cancel`),
  streamURL: BASE + "/api/stream",
};
