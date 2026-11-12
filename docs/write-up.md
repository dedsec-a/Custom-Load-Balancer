# Building a load balancer to see what scheduling actually buys you

I built a layer-7 load balancer in Go to answer one question: under overload, does it matter *which* request goes next? The short answer is yes, a lot. But the gain always comes out of someone else's latency, and the interesting part was measuring exactly whose.

## The setup

Three simulated backend servers, each with a fixed number of CPU cores and database connections. A request takes a core or a connection, holds it for its job time, and gives it back; if none is free, it waits. Three kinds of request:

- `/browse`: about 20 ms, mostly one database query (70% of traffic)
- `/checkout`: about 55 ms, three queries (10%)
- `/search`: 100 ms of pure CPU (20%)

In front of them sits the load balancer, with five strategies: round robin, least connections, first-come-first-served (FCFS), shortest-job-first (SJF) and an auto mode that switches between them. FCFS and SJF hold requests in the load balancer's own queue when every backend is full, so the scheduler, not the backend, decides the order.

## Fair comparisons

Watching a live dashboard is a good way to fool yourself, because one noisy second looks like a trend. So I built an experiment runner:

- every configuration gets the same load (150 virtual users) and the **same request sequence**, from a seeded random generator
- a 5-second warm-up, then a 30-second measurement window
- the order of configurations rotates between repeats, so none always runs first
- results are the median of 3 repeats, with the range kept

## What I found

| Configuration | Requests/s | `/browse` p95 | `/search` p99 |
|---|---:|---:|---:|
| Round robin | 231 | 1,451 ms | 1,041 ms |
| Least connections | 235 | 955 ms | 689 ms |
| FCFS | 232 | 787 ms | 867 ms |
| SJF (aging 0.02) | 234 | 174 ms | 2,819 ms |

**Throughput didn't move.** Every strategy completed about 232 requests per second, because the servers were busy either way. A scheduler doesn't make work disappear; it only decides who waits. That reframed the whole project for me.

**SJF cut `/browse` p95 by 78%** and made `/search` p99 3.3× worse. That isn't a bug, it's the deal: letting short jobs skip the queue means long jobs wait for every short job that arrives after them.

**Least connections beat round robin on every endpoint.** Round robin sends each server its turn even when one is stuck behind slow `/search` jobs. Least connections notices.

## The surprises

**1. My SJF was running the longest jobs first.** The first version estimated job length by timing each request at the load balancer. Under heavy load, most of that time was spent queueing *inside* the backend, so the estimates converged and even inverted: `/browse` looked slower than `/search`. SJF faithfully ran the "shortest" jobs, which were really the longest, and throughput collapsed to 70 requests/s. The fix was to have backends report their pure processing time in the standard `Server-Timing` header and learn from that. The dashboard now plots estimated vs measured job size, so a drift like that would be visible.

**2. Pure SJF "won" on throughput by dropping work.** Without aging, SJF showed 256 requests/s against FCFS's 236, which looked like a free win. It wasn't: about 230 `/search` requests per run waited the full 10-second queue timeout and were dropped. They never completed, so they never counted against throughput. Aging (each second of waiting makes a job count as 20 ms shorter) brought that to zero, with a 2.9-second worst case.

**3. Shed requests made overload look fast.** The first dashboard averaged latency over every request, including ones rejected in microseconds by a full queue. The worse the overload, the better the p50 looked. Now latency counts successful requests only, and failures are counted separately by reason.

**4. "Connection refused" at 640 users wasn't my code.** The stress tool opened 640 connections in the same instant and overflowed the operating system's accept queue. Spreading client starts over two seconds took errors from 64% to 0. The error breakdown in the tool is what pointed at it.

**5. Queueing theory held up, once I used the right model.** I fed Poisson arrivals into a single FIFO queue in front of all six CPU cores and compared the measured queue waits with the textbook. The classic M/M/c (Erlang C) formula predicted **twice** the wait I measured, at every load. The reason is its assumption that job times are random (exponential), while mine were nearly constant (100.5 ms every time). Constant job times halve the queueing, and the Allen–Cunneen correction for M/G/c says exactly that. With the correction, measurement and theory agreed **within 3% from 68% to 91% utilization**, and the share of requests that had to queue matched Erlang C almost exactly (57% vs 58% at 83%). They diverged where you'd expect: under about 3 ms, timer noise is as large as the effect, and at 94% a 45-second window is too short for the rare bursts that dominate the wait.

## Behaving like a real load balancer

Scheduling is half of it. The rest is failure handling: active and passive health checks (a dead backend was ejected 0.6 s after failing, before the next poll), one retry on a different backend within a budget of about 20% of traffic (killing one of three backends under load produced zero failed requests), a bounded queue that sheds load with `503` + `Retry-After`, draining when a backend is removed, per-IP rate limits, and Prometheus metrics with structured logs.

## What I'd do next

- Real CPU work in the backends instead of sleeps, to see how much the clean results depend on the simulation.
- Several load-balancer instances sharing health state.
- Size-based routing (send long jobs to dedicated backends) as an alternative to SJF's priority queue.
