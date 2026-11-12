# L7 load balancer with live traffic control

```
 virtual users (loadgen) ──► :8080  LB (HTTP reverse proxy) ──► b1 :3001
 real clients            ──►        scheduler: RR | FCFS | SJF  ──► b2 :3002
                                    auto picks one per second   ──► b3 :3003
 front-end ◄──────────────► :8081  admin API + SSE metrics stream
```

## Run locally

```powershell
.\scripts\dev.ps1                       # 3 backends (2 cores, 5 DB conns each) + LB
.\scripts\dev.ps1 -Backends 4 -Cores 4 -DBPool 10 -Algorithm sjf
go test ./...
```

## Algorithms

| name          | what it does                                                                                    |
|---------------|-------------------------------------------------------------------------------------------------|
| `round_robin` | forwards every request immediately to the next healthy backend. No LB queue.                    |
| `fcfs`        | each backend gets `cores + db_pool` slots; extra requests wait in the LB queue, oldest first, and go to the least-loaded backend. |
| `sjf`         | same queue, but the request with the shortest expected job runs first. Job length is learned per path from the backend's `Server-Timing: work;dur=` header. `aging` stops long jobs starving. |
| `auto`        | round robin while utilization < 90%. Above that: SJF if the slowest path costs ≥ 2× the fastest, otherwise FCFS. Back to round robin below 60% with an empty queue. |

## Admin API (port 8081)

| method & path                          | body / query                                                    |
|----------------------------------------|-----------------------------------------------------------------|
| `GET /api/state`                       | full state: algorithm, queue, backends, load, estimates         |
| `PUT /api/algorithm`                   | `{"algorithm":"sjf","aging":0.02,"max_queue":10000,"queue_timeout_ms":10000}` (all optional) |
| `GET /api/load` / `PUT /api/load`      | `{"users":200,"think_ms":0,"mix":{"/browse":70,"/search":20,"/checkout":10}}` (merge) |
| `PUT /api/resources`                   | `{"cores":8,"db_pool":20,"max_inflight":0}` → every backend (0 = auto slots) |
| `PUT /api/backends/{name}/resources`   | same, one backend                                               |
| `GET/POST /api/backends`, `DELETE /api/backends/{name}` | `{"name":"b4","url":"http://127.0.0.1:3004"}`  |
| `GET /api/metrics?last=120`            | last N one-second points                                        |
| `GET /api/stream`                      | SSE, `event: point` once per second                             |

Each point has `rps, p50_ms, p95_ms, p99_ms, errors, queue_len, utilization, effective, users`, plus per-backend `inflight/capacity/cores/db_pool/cpu_busy/db_busy` and per-path `p50/p95/est_ms/queued`.

Backend endpoints: `/browse` (~20 ms, DB-bound), `/checkout` (~55 ms), `/search` (100 ms, CPU-bound), `/health`, `/stats`, `PUT /admin/resources`.

## Stress testing

`cmd/stress` raises the number of concurrent clients step by step and reports req/s, p50/p90/p95/p99 and errors per step (with error types). It stops at the first step that breaks the SLO.

```powershell
go build -o bin/stress.exe ./cmd/stress
# whole system (simulated backend work)
.\bin\stress.exe -url http://127.0.0.1:8080/browse -steps 10,40,160,640 -slo-p95 250ms -csv results/stress.csv
# the LB's own limit: "/" does no simulated work, so only the proxying is measured
.\bin\stress.exe -url http://127.0.0.1:8080/ -steps 50,200,400 -slo-p95 50ms
```

Set dashboard users to 0 first, or the two traffic sources compete. Measured on one 24-core Windows machine, with load generator, LB and backends sharing it:

| test | result |
|---|---|
| `/browse`, 3 × (2 cores, 5 DB) | ~885 req/s max (theory 1000: DB-bound), p95 54 ms at 40 users, 0 errors up to 1280 users |
| `/browse`, 3 × (8 cores, 20 DB) | ~3740 req/s (theory 4000), scales ~4.2× with 4× DB |
| LB proxy only (`/`) | ~17–19k req/s, +0.6 ms p50 at low load (direct to backend: ~37–62k) |

Without `-ramp` (all clients connecting in the same instant), roughly 640 or more clients overflow the Windows accept backlog and get "connection refused". That's an OS connection-storm limit, not LB logic.

Keep port 8081 and the backends' `/admin` routes private when deployed. They have no auth yet.
