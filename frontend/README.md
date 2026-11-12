# Load balancer control panel

React + TypeScript + Vite + Recharts. Talks only to the load balancer's admin API (`:8081`).

## Run

```powershell
# terminal 1: backends + load balancer
cd backend
.\scripts\dev.ps1

# terminal 2: the UI
cd frontend
npm install      # first time only
npm run dev      # http://localhost:5173
```

In dev, Vite proxies `/api` to `127.0.0.1:8081`, so there is no CORS setup. To point a build at a load balancer somewhere else, set `VITE_API_BASE=http://host:8081` before `npm run build`.

## What's on the page

- **Traffic:** virtual users, think time, request mix (`PUT /api/load`).
- **Resources:** cores and DB pool for every backend (`PUT /api/resources`), or per backend from its card.
- **Algorithm:** Auto / Round robin / FCFS / SJF, plus the SJF aging slider (`PUT /api/algorithm`).
- **Charts:** response time (p50/p95), throughput, p95 per endpoint, LB queue length, and a load curve of p95 against users. Each change you make, auto-mode switches included, is drawn as a vertical line, and the tooltip names it.
- **Backends:** health, LB slots, CPU and DB pool usage with waiters, plus add and remove.
- **Results by configuration:** one row per configuration, with median throughput and p95.

## Code map

| file | what |
|---|---|
| `src/api.ts` | typed client for the admin API |
| `src/live.ts` | history + SSE stream hook; change markers and results rows computed from the metric points |
| `src/components/Controls.tsx` | sidebar controls (debounced, synced with the server) |
| `src/components/Charts.tsx` | time-series chart, load-curve scatter, sparkline |
| `src/components/Backends.tsx` | backend cards |
| `src/theme.ts` | chart palette for light/dark (colorblind-checked), formatters |
