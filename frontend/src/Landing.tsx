import { useEffect, useRef, useState, type ReactNode } from "react";
import { api } from "./api";
import { Link } from "./router";
import { usePalette } from "./theme";
import { algoLabel, type Limits, type Point, type State } from "./types";
import "./landing.css";

// ---- live status strip -----------------------------------------------------

function useLiveSummary() {
  const [state, setState] = useState<State | null>(null);
  const [last, setLast] = useState<Point | null>(null);
  const [down, setDown] = useState(false);
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const [s, m] = await Promise.all([api.state(), api.metrics(1)]);
        if (!alive) return;
        setState(s);
        setLast(m[m.length - 1] ?? null);
        setDown(false);
      } catch {
        if (alive) setDown(true);
      }
    };
    tick();
    const id = setInterval(tick, 3000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);
  return { state, last, down };
}

function LiveStrip() {
  const { state, last, down } = useLiveSummary();
  if (down) {
    return (
      <p className="lp-live down">
        <span aria-hidden>✕</span> The live system isn't reachable right now.
      </p>
    );
  }
  if (!state) return <p className="lp-live">◌ Connecting to the live system…</p>;
  const healthy = state.backends.filter((b) => b.healthy).length;
  return (
    <p className="lp-live ok">
      <span className="dot" aria-hidden>●</span> <b>Live right now:</b> {healthy} of {state.backends.length} backends healthy
      <span className="sep">·</span> running <b>{algoLabel(state.effective)}</b>
      {state.algorithm === "auto" && " (auto)"}
      <span className="sep">·</span> {state.load.users} virtual users
      <span className="sep">·</span> {Math.round(last?.rps ?? 0)} requests/s
    </p>
  );
}

// ---- small building blocks -------------------------------------------------

function Section({ id, kicker, title, children }: { id: string; kicker: string; title: string; children: ReactNode }) {
  return (
    <section id={id} className="lp-section">
      <p className="lp-kicker">{kicker}</p>
      <h2>{title}</h2>
      {children}
    </section>
  );
}

function DashLink({ children = "Try it in the dashboard →" }: { children?: ReactNode }) {
  return (
    <Link to="/dashboard" className="lp-inline-cta">
      {children}
    </Link>
  );
}

// ---- request journey (HTML/CSS so it reflows on phones) --------------------

function Journey() {
  return (
    <figure className="lp-flow" aria-label="Path of a request through the system">
      <div className="node">
        <span className="node-title">Your browser</span>
        <span className="node-sub">or any HTTP client</span>
      </div>
      <div className="arrow" aria-hidden>
        <span>HTTPS</span>
      </div>
      <div className="node">
        <span className="node-title">Caddy</span>
        <span className="node-sub">TLS certificate, the only public door</span>
      </div>
      <div className="arrow" aria-hidden />
      <div className="node accent">
        <span className="node-title">Load balancer</span>
        <span className="node-sub">health checks · queue · scheduler · metrics</span>
        <span className="queue" aria-hidden>
          <i className="j browse" />
          <i className="j search" />
          <i className="j checkout" />
          <i className="j browse" />
        </span>
      </div>
      <div className="arrow" aria-hidden />
      <div className="node stack">
        {["b1", "b2", "b3"].map((b) => (
          <span key={b} className="mini">
            <b>{b}</b> cores + DB pool
          </span>
        ))}
      </div>
    </figure>
  );
}

// ---- FCFS vs SJF worked example ---------------------------------------------

type Job = { id: string; path: "/browse" | "/checkout" | "/search"; ms: number };
const JOBS: Job[] = [
  { id: "A", path: "/search", ms: 100 },
  { id: "B", path: "/browse", ms: 20 },
  { id: "C", path: "/checkout", ms: 55 },
  { id: "D", path: "/browse", ms: 20 },
];
const TOTAL = JOBS.reduce((s, j) => s + j.ms, 0); // 195

function schedule(order: Job[]) {
  let t = 0;
  return order.map((j) => {
    const start = t;
    t += j.ms;
    return { ...j, start };
  });
}

function Timeline() {
  const pal = usePalette();
  const color: Record<Job["path"], string> = {
    "/browse": pal.series[0],
    "/checkout": pal.series[1],
    "/search": pal.series[2],
  };
  const rows = [
    { name: "FCFS", note: "in arrival order", runs: schedule(JOBS) },
    { name: "SJF", note: "shortest first", runs: schedule([...JOBS].sort((a, b) => a.ms - b.ms)) },
  ];
  return (
    <figure className="lp-timeline">
      <div className="lp-arrivals">
        <span className="muted">Arrive in this order, one free server:</span>
        {JOBS.map((j) => (
          <span key={j.id} className="chip-job">
            <span className="swatch" style={{ background: color[j.path] }} />
            <b>{j.id}</b> <code>{j.path}</code> {j.ms} ms
          </span>
        ))}
      </div>
      {rows.map((r) => {
        const avg = r.runs.reduce((s, x) => s + x.start, 0) / r.runs.length;
        return (
          <div className="tl-row" key={r.name}>
            <div className="tl-label">
              <b>{r.name}</b>
              <span>{r.note}</span>
            </div>
            <div className="tl-track" role="img" aria-label={`${r.name}: ${r.runs.map((x) => `${x.id} starts at ${x.start} ms`).join(", ")}`}>
              {r.runs.map((x) => (
                <div
                  key={x.id}
                  className="tl-seg"
                  style={{ width: `${(x.ms / TOTAL) * 100}%`, background: color[x.path] }}
                  title={`${x.id} ${x.path}: waits ${x.start} ms, runs ${x.ms} ms`}
                >
                  {x.id}
                </div>
              ))}
            </div>
            <div className="tl-avg">
              average wait <b>{Math.round(avg)} ms</b>
            </div>
          </div>
        );
      })}
      <div className="tl-axis" aria-hidden>
        <span />
        <div>
          <span>0</span>
          <span>{TOTAL} ms</span>
        </div>
        <span />
      </div>
      <figcaption>
        Both finish all the work at {TOTAL} ms. SJF just lets the short jobs out first: waits of 0, 20, 40 and 95 ms instead
        of 0, 100, 120 and 175 ms. The catch: job A, the long one, now waits the longest.
      </figcaption>
    </figure>
  );
}

// ---- metric glossary --------------------------------------------------------

function Glossary({ title, items }: { title: string; items: [string, ReactNode][] }) {
  return (
    <div className="lp-gloss">
      <h3>{title}</h3>
      <dl>
        {items.map(([term, def]) => (
          <div key={term}>
            <dt>{term}</dt>
            <dd>{def}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

// ---- measured results ----------------------------------------------------------

/** Aging result, from the before/after experiment (150 users, median of 3 runs). */
function AgingNote() {
  return (
    <>
      {" "}Without aging it is worse: <code>/search</code> p99 reached 8.2 s and about 230 requests per 30 s run waited the
      full 10 s queue timeout and were dropped. Aging 0.02 brought that to 0 dropped and a 2.9 s worst case; aging 0.1 to
      1.7 s, giving back most of the <code>/browse</code> gain. <Link to="/experiments">See the experiment</Link>.
    </>
  );
}

const COMPARISON = [
  { name: "Round robin", rps: 231, browse: 1451, search99: 1041 },
  { name: "Least connections", rps: 235, browse: 955, search99: 689 },
  { name: "FCFS", rps: 232, browse: 787, search99: 867 },
  { name: "SJF (aging 0.02)", rps: 234, browse: 174, search99: 2819 },
];

function ComparisonTable() {
  return (
    <div className="lp-table-wrap">
      <table className="lp-table">
        <thead>
          <tr>
            <th>150 users, 3 backends × (2 cores, 5 DB)</th>
            <th className="r">Requests/s</th>
            <th className="r">/browse p95</th>
            <th className="r">/search p99</th>
          </tr>
        </thead>
        <tbody>
          {COMPARISON.map((r) => (
            <tr key={r.name}>
              <td>{r.name}</td>
              <td className="r">{r.rps}</td>
              <td className="r">{r.browse.toLocaleString()} ms</td>
              <td className="r">{r.search99.toLocaleString()} ms</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="lp-table-note">
        Same throughput everywhere: the schedulers don't do more work, they decide who waits. Capacity below.
      </p>
    </div>
  );
}

// ---- page -------------------------------------------------------------------

export default function Landing() {
  const heroCta = useRef<HTMLSpanElement>(null);
  const [showFloat, setShowFloat] = useState(false);
  const [limits, setLimits] = useState<Limits | undefined>();

  // Floating CTA appears once the hero button scrolls out of view.
  useEffect(() => {
    const el = heroCta.current;
    if (!el) return;
    const io = new IntersectionObserver(([e]) => setShowFloat(!e.isIntersecting), { threshold: 0 });
    io.observe(el);
    return () => io.disconnect();
  }, []);

  // Honor /#section links coming from the dashboard.
  useEffect(() => {
    const id = window.location.hash.slice(1);
    if (id) requestAnimationFrame(() => document.getElementById(id)?.scrollIntoView());
    api.state().then((s) => setLimits(s.limits)).catch(() => {});
  }, []);

  const maxUsers = limits?.max_users || 2000;

  return (
    <div className="lp">
      <header className="lp-nav">
        <a href="#top" className="lp-brand">
          <svg width="22" height="22" viewBox="0 0 32 32" aria-hidden>
            <circle cx="6" cy="16" r="4" />
            <circle cx="26" cy="6" r="3" />
            <circle cx="26" cy="16" r="3" />
            <circle cx="26" cy="26" r="3" />
            <path d="M10 16L23 6M10 16H23M10 16L23 26" />
          </svg>
          Load Balancer
        </a>
        <nav className="lp-links" aria-label="Sections">
          <a href="#what">Basics</a>
          <a href="#algorithms">Algorithms</a>
          <a href="#metrics">Metrics</a>
          <a href="#infra">Docker &amp; AWS</a>
        </nav>
        <Link to="/dashboard" className="lp-btn small">
          Dashboard
        </Link>
      </header>

      <main>
        {/* ---- hero ---- */}
        <section id="top" className="lp-hero">
          <p className="lp-eyebrow">A layer-7 load balancer in Go · React · Docker · AWS</p>
          <h1>Shortest-job-first cut short-request latency 78% under overload.</h1>
          <p className="lp-lede">
            And it made the longest jobs wait <b>3× longer</b>. That trade-off, measured in controlled experiments, is what
            this project is about: a working load balancer with five scheduling strategies, failover, retries and load
            shedding, that you can push traffic at yourself and watch react live.
          </p>
          <div className="lp-hero-ctas">
            <Link to="/dashboard" className="lp-btn">
              <span ref={heroCta}>Open the live dashboard →</span>
            </Link>
            <Link to="/experiments" className="lp-btn ghost">
              See the experiments
            </Link>
            <a href="#what" className="lp-btn ghost">
              How it works ↓
            </a>
          </div>
          <LiveStrip />
        </section>

        {/* ---- basics ---- */}
        <Section id="what" kicker="The basics" title="What is a load balancer?">
          <p>
            One server can only do so much. When a site gets busy you run several copies of it, and something has to decide
            which copy handles each request. That something is the <b>load balancer</b>. Think of the host at a busy
            restaurant: every guest comes through the door, and the host decides which table they go to so no waiter is
            swamped while another stands idle.
          </p>
          <div className="lp-cards three">
            <div className="lp-card">
              <h3>Spread the work</h3>
              <p>Send each request to a server that can take it, so no single machine becomes the bottleneck.</p>
            </div>
            <div className="lp-card">
              <h3>Skip broken servers</h3>
              <p>Check every server's health each second. A server that stops answering stops getting traffic.</p>
            </div>
            <div className="lp-card">
              <h3>Queue when everyone is busy</h3>
              <p>
                When every server is full, hold requests in a queue and decide <i>which one</i> goes next. That decision is
                the scheduling algorithm.
              </p>
            </div>
          </div>
          <aside className="lp-callout">
            <b>Layer 4 vs layer 7.</b> A layer-4 balancer only sees IP addresses and ports. This one works at
            <b> layer 7 (HTTP)</b>: it reads every request, so it knows whether you asked for a quick <code>/browse</code> or
            a heavy <code>/search</code> and can treat them differently. That's what makes shortest-job-first possible.
          </aside>
        </Section>

        {/* ---- project ---- */}
        <Section id="project" kicker="This project" title="What's running behind the dashboard">
          <div className="lp-cards two">
            <div className="lp-card">
              <h3>The load balancer (Go)</h3>
              <p>
                A reverse proxy with a pluggable scheduler. It health-checks backends every second, learns how long each URL
                takes, queues requests when the backends are full, and publishes one metrics point per second.
              </p>
            </div>
            <div className="lp-card">
              <h3>Three backend servers (Go)</h3>
              <p>
                Each one simulates a server with a limited number of <b>CPU cores</b> and a <b>database connection pool</b>.
                Requests wait for a free core or connection, just like on a real machine. You can resize both while it runs.
              </p>
              <ul className="lp-endpoints">
                <li>
                  <code>/browse</code> about 20 ms, mostly one DB query
                </li>
                <li>
                  <code>/checkout</code> about 55 ms, three DB queries
                </li>
                <li>
                  <code>/search</code> 100 ms of pure CPU
                </li>
              </ul>
            </div>
            <div className="lp-card">
              <h3>A traffic generator</h3>
              <p>
                "Virtual users" that each send a request, wait for the answer, optionally pause, and repeat. You choose how
                many there are and which URLs they hit. Their traffic goes through the load balancer exactly like real traffic.
              </p>
            </div>
            <div className="lp-card">
              <h3>The dashboard (React)</h3>
              <p>
                Controls for traffic, resources and algorithm, plus charts that update every second over a live stream
                (Server-Sent Events). Every change you make is marked on the charts.
              </p>
            </div>
          </div>

          <h3 className="lp-h3">The journey of one request</h3>
          <Journey />
          <ol className="lp-steps">
            <li>Your browser connects over HTTPS to <b>Caddy</b>, which holds the certificate and is the only thing exposed to the internet.</li>
            <li>Caddy hands the request to the <b>load balancer</b>, which only considers backends that passed their last health check.</li>
            <li>
              The <b>scheduler</b> picks a backend. If they're all at capacity, the request waits in the load balancer's queue
              until the algorithm says it's its turn.
            </li>
            <li>The <b>backend</b> does its simulated work: it waits for a free core or DB connection, then holds it for the job's duration.</li>
            <li>
              On the way back, the load balancer records the latency, which backend served it, how long it queued, and the pure
              work time the backend reports in a <code>Server-Timing</code> header.
            </li>
          </ol>
        </Section>

        {/* ---- algorithms ---- */}
        <Section id="algorithms" kicker="Scheduling" title="Five ways to decide who goes next">
          <p>
            Every algorithm answers two questions: <b>which waiting request goes next</b>, and <b>which server gets it</b>.
            Here's the difference between the two queued algorithms on one small example:
          </p>
          <Timeline />

          <div className="lp-algos">
            <article className="lp-algo">
              <header>
                <h3>Round robin</h3>
                <span className="tag">Take turns</span>
              </header>
              <p>
                Requests go to b1, b2, b3, b1, b2… the moment they arrive. The load balancer never holds anything back; each
                backend queues whatever it's given.
              </p>
              <p className="pro"><b>Good:</b> dead simple, zero overhead, fair when every request and server is alike.</p>
              <p className="con">
                <b>Weak:</b> it can't see load. A backend stuck with slow jobs keeps receiving its share. Under overload in our
                tests, one backend held 113 requests while another had 5.
              </p>
            </article>

            <article className="lp-algo">
              <header>
                <h3>Least connections</h3>
                <span className="tag">Fewest busy first</span>
              </header>
              <p>
                Each request goes at once to the backend with the fewest requests in flight, relative to its slots
                (weighted least connections, the default in most production load balancers). Ties rotate.
              </p>
              <p className="pro">
                <b>Good:</b> sees load, so a backend stuck with slow jobs stops attracting more. In the comparison it beat
                round robin on every endpoint: <code>/browse</code> p95 955 ms vs 1,451 ms.
              </p>
              <p className="con"><b>Weak:</b> no LB queue, so it can't choose <i>which</i> request goes next.</p>
            </article>

            <article className="lp-algo">
              <header>
                <h3>FCFS</h3>
                <span className="tag">First come, first served</span>
              </header>
              <p>
                Each backend gets a number of <b>slots</b> (its cores + DB connections). When every slot is taken, new requests
                wait in one shared queue at the load balancer. When a slot frees up, the <b>oldest</b> request goes to the
                least-busy backend.
              </p>
              <p className="pro"><b>Good:</b> fair and predictable; keeps backends from being buried.</p>
              <p className="con"><b>Weak:</b> a 20 ms request stuck behind a few 100 ms ones waits for all of them.</p>
            </article>

            <article className="lp-algo">
              <header>
                <h3>SJF</h3>
                <span className="tag">Shortest job first</span>
              </header>
              <p>
                Same queue and slots, but the next request out is the one with the <b>smallest expected job</b>. Job sizes
                are learned per URL from what the backends report, as a moving average.
              </p>
              <p>
                <b>Aging</b> keeps long jobs from waiting forever: each millisecond spent waiting lowers a request's score by
                <i> aging</i> × 1 ms. At the default 0.02, one second of waiting makes a job count as 20 ms shorter.
              </p>
              <p className="pro">
                <b>Good:</b> short jobs fly. At 150 users, <code>/browse</code> p95 fell from 787 ms (FCFS) to 174 ms, a 78% cut,
                with the same throughput.
              </p>
              <p className="con">
                <b>Weak:</b> long jobs pay for it: <code>/search</code> p99 rose from 867 ms to 2,819 ms (+225%).
                <AgingNote />
              </p>
            </article>

            <article className="lp-algo">
              <header>
                <h3>Auto</h3>
                <span className="tag">Decide every second</span>
              </header>
              <p>
                Measures <b>utilization</b> = (requests in flight + queued) ÷ total slots, and the size gap between the
                slowest and fastest URL.
              </p>
              <ul>
                <li>Below 90% utilization: <b>round robin</b> (no queue needed).</li>
                <li>Above 90%: <b>SJF</b> if the slowest URL costs at least 2× the fastest, otherwise <b>FCFS</b>.</li>
                <li>Back to round robin below 60% with an empty queue.</li>
              </ul>
              <p className="muted small">
                The gap between 60% and 90% stops it flip-flopping. The dashboard shows the reason for its current choice.
              </p>
            </article>
          </div>

          <aside className="lp-callout">
            <b>How does it know how long a job takes?</b> The first version timed each request at the load balancer. Under
            heavy load that time was mostly <i>waiting</i> inside the backend, so <code>/browse</code> started to look slower
            than <code>/search</code> and SJF ran the longest jobs first. Throughput dropped to 70 requests/s. The fix: each
            backend reports its pure work time in the standard <code>Server-Timing</code> header (<code>work;dur=20.1</code>),
            and the load balancer learns from that instead.
          </aside>
        </Section>

        {/* ---- resilience ---- */}
        <Section id="resilience" kicker="Failure handling" title="It behaves like a real load balancer">
          <p>
            Scheduling is only half the job. The other half is what happens when a backend breaks or there is more traffic
            than capacity. Every behaviour below is visible on the dashboard; press <b>Simulate outage</b> on a backend card
            to watch it.
          </p>
          <div className="lp-cards two">
            <div className="lp-card">
              <h3>Health checks: active and passive</h3>
              <p>
                <b>Active:</b> every backend is polled each second; two failures in a row mark it down. <b>Passive:</b> three
                failed real requests in a row eject it at once, without waiting for the next poll, for a 10 s cool-down. In
                our test the passive check ejected a dead backend 0.6 s after it failed.
              </p>
            </div>
            <div className="lp-card">
              <h3>Retries with a budget</h3>
              <p>
                A GET that fails (connection error, timeout, 502/503/504) is retried once on a <i>different</i> backend.
                Retries are capped at about 20% of traffic so a sick cluster isn't hit by a retry storm, and POSTs are never
                retried. Killing one of three backends under load: <b>0 failed requests</b>.
              </p>
            </div>
            <div className="lp-card">
              <h3>Backpressure and load shedding</h3>
              <p>
                The FCFS/SJF queue is bounded in length and in time. When it is full, or a request waited too long, the
                request is turned away with 503 and <code>Retry-After</code> instead of piling up. With a 30-request queue at
                150 users, accepted requests kept a p95 under 400 ms instead of several seconds.
              </p>
            </div>
            <div className="lp-card">
              <h3>Timeouts, draining, rate limits</h3>
              <p>
                Upstream connect and response timeouts; removing a backend <b>drains</b> it (no new requests, removed once its
                in-flight requests finish); per-IP rate limits on both the public traffic port and the control API.
              </p>
            </div>
          </div>
        </Section>

        {/* ---- metrics ---- */}
        <Section id="metrics" kicker="Reading the dashboard" title="What every number means">
          <p>
            Everything is measured over <b>one-second windows</b> and updates live. Latency is measured end to end through the
            load balancer, including any time a request spent in its queue.
          </p>
          <aside className="lp-callout soft">
            <b>Why p95 and not the average?</b> Averages hide the slow requests people actually notice. <b>p95</b> means 95%
            of requests finished at least this fast and the slowest 5% took longer. <b>p50</b> (the median) is the
            typical request. When p95 pulls away from p50, some requests are stuck waiting.
          </aside>
          <div className="lp-gloss-grid">
            <Glossary
              title="Top tiles"
              items={[
                ["Users", "Virtual users running. Each sends a request, waits for the reply, pauses for the think time, and repeats."],
                ["Requests/s", "Throughput: responses completed in the last second."],
                ["p95 latency", "95% of the last second's requests finished at least this fast."],
                ["Queued at LB", "Requests waiting in the load balancer's queue for a free slot. Always 0 under round robin."],
                [
                  "Utilization",
                  "(In flight + queued) ÷ total slots. Above 100% means more work than the servers can hold at once; the rest waits.",
                ],
                ["Failed / shed /s", "Failed = a backend error or no backend left after retries. Shed = turned away by the bounded queue (full or waited too long). Latency figures count successful requests only."],
                ["Capacity", "Total CPU cores and DB connections across all healthy backends."],
                ["Algorithm", "What's actually running. Under Auto it changes by itself, and the vertical markers show when."],
              ]}
            />
            <Glossary
              title="Charts"
              items={[
                ["Response time", "p50 and p95 latency over time."],
                ["Throughput", "Requests completed per second. When it flattens while users keep rising, you've hit capacity."],
                ["p95 by endpoint", "One line per URL. This is where SJF's trade-off shows: short URLs drop, long ones rise."],
                ["LB queue", "How many requests are waiting at the load balancer."],
                ["Failed, shed and retried", "Load shedding and failures per second, plus requests that were retried on another backend after one failed."],
                ["Job size: estimated vs measured", "SJF's learned estimate per URL (solid) against the work time backends actually report (dashed). If they drift apart, SJF is ordering by the wrong numbers."],
                [
                  "Load curve",
                  "p95 latency against number of users, one dot per second. The current setup is highlighted and earlier setups are gray, so you can see how a change shifts the curve.",
                ],
                ["Vertical lines", "A change: users, resources, algorithm, or an automatic switch. Hover a chart to see which."],
              ]}
            />
            <Glossary
              title="Backend cards"
              items={[
                ["Healthy / Down / Ejected / Draining", "Down = failing active checks or a simulated outage. Ejected = removed by passive checks after failed real requests, back after a 10 s cool-down. Draining = finishing its requests before removal."],
                ["Simulate outage", "Makes that backend answer 503 for 60 s. Watch the ejection, the retries and the recovery."],
                ["LB slots", "Requests in flight on this backend ÷ its slots (cores + DB pool)."],
                ["CPU cores", "Busy cores ÷ total. \"Waiting\" = requests inside this backend waiting for a core."],
                ["DB connections", "Busy connections ÷ pool size, plus requests waiting for one."],
                ["Sparkline", "Requests in flight over time; the gray rule is the slot limit."],
              ]}
            />
            <Glossary
              title="Controls"
              items={[
                ["Virtual users", `How many simulated users are active (up to ${maxUsers} here).`],
                ["Think time", "Pause between one user's requests. 0 means they fire again the moment they get an answer."],
                ["Request mix", "What share of requests go to /browse, /checkout and /search."],
                ["CPU cores / DB pool", "Resize every backend live. More DB connections help /browse and /checkout; more cores help /search."],
                ["SJF aging", "Left = favor short jobs harder. Right = fairer to long jobs."],
                ["Max queue / queue timeout", "Backpressure. Lower them to see load shedding instead of ever-growing waits."],
                ["Results table", "One row per configuration you tried, with median throughput and p95, to compare runs."],
              ]}
            />
          </div>
        </Section>

        {/* ---- infra ---- */}
        <Section id="infra" kicker="Where it runs" title="Docker and AWS">
          <div className="lp-cards two">
            <div className="lp-card">
              <h3>Docker: five containers, one image</h3>
              <p>
                <b>caddy</b>, <b>lb</b>, <b>b1</b>, <b>b2</b> and <b>b3</b> each run in their own container, started together
                by Docker Compose. The image is built in three stages: the dashboard is built with Node, the Go code is
                tested and compiled, and only the results are copied into a small Alpine image (about 34 MB).
              </p>
              <ul>
                <li>The load balancer reaches the backends over a real network, like separate machines.</li>
                <li>If a backend crashes, Docker restarts just that one.</li>
                <li>Only Caddy has public ports; the backends and the control API are on a private network.</li>
              </ul>
            </div>
            <div className="lp-card">
              <h3>AWS: one small EC2 instance</h3>
              <p>
                The live demo runs on a single <b>EC2 t2.micro</b> (1 vCPU, 1 GB RAM) with Ubuntu. Its security group
                allows only web traffic (ports 80 and 443) from everyone, and SSH from one address.
              </p>
              <ul>
                <li>Caddy gets a free HTTPS certificate from Let's Encrypt automatically and renews it.</li>
                <li>
                  The hostname comes from <b>sslip.io</b>, which turns an IP address into a domain name, so no domain
                  purchase is needed.
                </li>
                <li>The whole stack uses about 28 MB of RAM idle and about 92 MB at full demo load.</li>
              </ul>
            </div>
          </div>
          <div className="lp-cards three">
            <div className="lp-card">
              <h3>CI/CD</h3>
              <p>
                GitHub Actions runs gofmt, vet and the tests with the race detector, a <b>performance gate</b> that fails if
                p95 regresses or SJF stops helping short jobs, the front-end build and the Docker build; a deploy workflow
                ships the images to EC2 and smoke-tests the site.
              </p>
            </div>
            <div className="lp-card">
              <h3>Infrastructure as code</h3>
              <p>
                Terraform creates the instance, security group, a fixed Elastic IP and an optional budget alert, with CPU
                credits pinned to "standard" and IMDSv2 only. One <code>terraform apply</code> rebuilds the whole setup.
              </p>
            </div>
            <div className="lp-card">
              <h3>Observability</h3>
              <p>
                Prometheus metrics at <code>/metrics</code> (latency histograms, outcomes, retries, backend health), a
                ready-made Grafana dashboard, and structured JSON logs for every ejection, recovery and admin change.
              </p>
            </div>
          </div>
          <aside className="lp-callout soft">
            <b>Public-demo guardrails.</b> Because anyone can use this page: at most {limits?.max_users || 150} virtual users,
            {" "}{limits?.max_cores || 8} cores and {limits?.max_db_pool || 50} DB connections per backend; virtual users stop
            automatically after {Math.round((limits?.idle_stop_s || 600) / 60)} minutes without a change; and adding or removing
            backends is switched off, so nobody can point the server at an arbitrary address.
          </aside>
        </Section>

        {/* ---- numbers ---- */}
        <Section id="numbers" kicker="Evidence" title="Measured, not claimed">
          <p>
            Algorithm comparisons come from the <Link to="/experiments">experiments page</Link>: same load, same seeded
            request sequence, 30 s runs after a warm-up, median of 3 repeats with their range. Capacity numbers come from a
            separate stress-test tool. All measured on a 24-core desktop; the live t2.micro is much smaller.
          </p>
          <ComparisonTable />
          <div className="lp-table-wrap">
            <table className="lp-table">
              <thead>
                <tr>
                  <th>Setup</th>
                  <th className="r">Result</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td>3 backends × (2 cores, 5 DB), <code>/browse</code></td>
                  <td className="r">
                    <b>≈ 885 req/s</b> max (theory: 1000, DB-bound); 0 errors even at 1,280 users
                  </td>
                </tr>
                <tr>
                  <td>3 backends × (8 cores, 20 DB), <code>/browse</code></td>
                  <td className="r">
                    <b>≈ 3,740 req/s</b>, 4.2× more for 4× the DB connections
                  </td>
                </tr>
                <tr>
                  <td>Load balancer alone (no backend work)</td>
                  <td className="r">
                    <b>≈ 18,000 req/s</b>, adds about 0.6 ms per request
                  </td>
                </tr>
                <tr>
                  <td>SJF vs FCFS under overload</td>
                  <td className="r">
                    <code>/browse</code> median <b>680 → 67 ms</b>; <code>/search</code> waits ≈ 2.9 s
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </Section>

        {/* ---- experiments ---- */}
        <Section id="try" kicker="Experiments" title="Five things to try">
          <ol className="lp-try">
            <li>
              <h3>Find the breaking point</h3>
              <p>
                Click <b>10</b>, then <b>50</b>, then <b>150</b> users. Throughput climbs, then flattens; latency keeps
                rising. That bend is the system's capacity.
              </p>
            </li>
            <li>
              <h3>Buy your way out</h3>
              <p>At 150 users, drag <b>CPU cores</b> to 8 and <b>DB pool</b> to 20. Watch p95 fall and throughput jump at the marker.</p>
            </li>
            <li>
              <h3>Speed vs fairness</h3>
              <p>
                Pick <b>SJF</b> at 150 users and watch <b>p95 by endpoint</b> split apart. Then drag <b>SJF aging</b> right
                and watch the lines come back together.
              </p>
            </li>
            <li>
              <h3>Pull the plug</h3>
              <p>
                At 50 users, press <b>Simulate outage</b> on b2. Watch it turn red within a second, the retried line jump,
                failed stay at 0, and b2 come back when the outage ends.
              </p>
            </li>
            <li>
              <h3>Let it decide</h3>
              <p>Choose <b>Auto</b> and ramp users up and down. Each switch shows up as a marker, with the reason under the algorithm buttons.</p>
            </li>
          </ol>
          <DashLink />
        </Section>
      </main>

      <footer className="lp-footer">
        <p>Built with Go, React, TypeScript, Recharts, Docker, Caddy and AWS EC2.</p>
        <Link to="/dashboard">Open the dashboard →</Link>
      </footer>

      <Link to="/dashboard" className={`lp-float${showFloat ? " show" : ""}`}>
        Open dashboard →
      </Link>
    </div>
  );
}
