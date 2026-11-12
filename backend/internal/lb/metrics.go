package lb

import (
	"slices"
	"sync"
	"time"
)

// Outcome is what happened to one request.
type Outcome uint8

const (
	OutcomeOK           Outcome = iota
	OutcomeBackendError         // backend failed or answered 5xx (after any retry)
	OutcomeNoBackend            // no healthy backend to send it to
	OutcomeQueueFull            // shed: the bounded LB queue was full
	OutcomeQueueTimeout         // shed: waited in the LB queue too long
	OutcomeRateLimited          // per-IP rate limit
	OutcomeClientGone           // client disconnected first (not counted as an error)
)

// Point is one second of history: what the front-end graphs.
// Latency figures cover successful requests only, so shed requests (which
// fail in microseconds) can't make an overload look fast.
type Point struct {
	T            int64                `json:"t"`        // unix ms
	RPS          float64              `json:"rps"`      // successful responses per second
	Requests     int                  `json:"requests"` // successful responses this second
	Errors       int                  `json:"errors"`   // backend errors + no backend available
	Shed         int                  `json:"shed"`     // rejected by the bounded queue (full or timed out)
	RateLimited  int                  `json:"rate_limited"`
	Retries      int                  `json:"retries"` // requests retried on another backend
	P50Ms        float64              `json:"p50_ms"`
	P95Ms        float64              `json:"p95_ms"`
	P99Ms        float64              `json:"p99_ms"`
	AvgMs        float64              `json:"avg_ms"`
	QueueWaitP95 float64              `json:"queue_wait_p95_ms"`
	QueueLen     int                  `json:"queue_len"`
	Utilization  float64              `json:"utilization"`
	Algorithm    Algorithm            `json:"algorithm"`
	Effective    Algorithm            `json:"effective"`
	Aging        float64              `json:"aging"`
	Users        int                  `json:"users"`
	Backends     []BackendPoint       `json:"backends"`
	Paths        map[string]PathPoint `json:"paths"`
}

type BackendPoint struct {
	Name       string `json:"name"`
	Healthy    bool   `json:"healthy"`
	Ejected    bool   `json:"ejected"`
	Draining   bool   `json:"draining"`
	Down       bool   `json:"down"` // simulated outage in progress
	Requests   int    `json:"requests"`
	Inflight   int    `json:"inflight"`
	Capacity   int    `json:"capacity"`
	Cores      int    `json:"cores"`
	DBPool     int    `json:"db_pool"`
	CPUBusy    int    `json:"cpu_busy"`
	CPUWaiting int    `json:"cpu_waiting"`
	DBBusy     int    `json:"db_busy"`
	DBWaiting  int    `json:"db_waiting"`
}

type PathPoint struct {
	Requests int     `json:"requests"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	EstMs    float64 `json:"est_ms"`  // the scheduler's estimate (EWMA) at the end of the second
	WorkMs   float64 `json:"work_ms"` // measured work time this second (median, from Server-Timing)
	Queued   int     `json:"queued"`
}

// Sample is one finished request, as seen by an experiment tap.
type Sample struct {
	Path    string
	Latency time.Duration // end to end, including LB queue wait
	Wait    time.Duration // time in the LB queue
	Work    time.Duration // backend's own processing time (0 = unknown)
	Outcome Outcome
}

type pathWin struct{ lat, work []float64 }

type window struct {
	lat         []float64
	qwait       []float64
	errors      int
	shed        int
	rateLimited int
	retries     int
	byBackend   map[string]int
	byPath      map[string]*pathWin
}

func newWindow() window {
	return window{byBackend: map[string]int{}, byPath: map[string]*pathWin{}}
}

const maxPathsPerPoint = 32

// Collector aggregates per-request records into 1-second Points and keeps
// a ring of recent history plus live subscribers (for SSE).
type Collector struct {
	mu      sync.Mutex
	cur     window
	since   time.Time
	history []Point
	keep    int
	subs    map[chan Point]struct{}
	tap     func(Sample) // experiment recorder, if one is running
	prom    *promCounters
}

func NewCollector(keep int) *Collector {
	return &Collector{cur: newWindow(), since: time.Now(), keep: keep, subs: map[chan Point]struct{}{}, prom: newPromCounters()}
}

// SetTap installs (or, with nil, removes) a function that sees every request.
func (c *Collector) SetTap(f func(Sample)) {
	c.mu.Lock()
	c.tap = f
	c.mu.Unlock()
}

func (c *Collector) pathLocked(path string) *pathWin {
	pw := c.cur.byPath[path]
	if pw == nil {
		if len(c.cur.byPath) >= maxPathsPerPoint {
			path = "(other)"
			if pw = c.cur.byPath[path]; pw != nil {
				return pw
			}
		}
		pw = &pathWin{}
		c.cur.byPath[path] = pw
	}
	return pw
}

// Record counts one finished request. work = the backend's processing time
// for successful requests (0 when unknown or failed).
func (c *Collector) Record(path, backend string, latency, queueWait, work time.Duration, outcome Outcome) {
	c.prom.record(path, latency, queueWait, outcome)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tap != nil {
		c.tap(Sample{Path: path, Latency: latency, Wait: queueWait, Work: work, Outcome: outcome})
	}
	switch outcome {
	case OutcomeOK:
		ms := float64(latency) / 1e6
		c.cur.lat = append(c.cur.lat, ms)
		c.cur.qwait = append(c.cur.qwait, float64(queueWait)/1e6)
		if backend != "" {
			c.cur.byBackend[backend]++
		}
		pw := c.pathLocked(path)
		pw.lat = append(pw.lat, ms)
		if work > 0 {
			pw.work = append(pw.work, float64(work)/1e6)
		}
	case OutcomeBackendError, OutcomeNoBackend:
		c.cur.errors++
	case OutcomeQueueFull, OutcomeQueueTimeout:
		c.cur.shed++
	case OutcomeRateLimited:
		c.cur.rateLimited++
	}
}

// Retry counts one request retried on another backend.
func (c *Collector) Retry() {
	c.prom.retry()
	c.mu.Lock()
	c.cur.retries++
	c.mu.Unlock()
}

// Flush closes the current second and returns its Point.
func (c *Collector) Flush(now time.Time, st BalancerState, est map[string]PathEstimate, users int) Point {
	c.mu.Lock()
	w := c.cur
	elapsed := now.Sub(c.since).Seconds()
	c.cur, c.since = newWindow(), now
	c.mu.Unlock()

	p := Point{
		T:           now.UnixMilli(),
		Requests:    len(w.lat),
		Errors:      w.errors,
		Shed:        w.shed,
		RateLimited: w.rateLimited,
		Retries:     w.retries,
		QueueLen:    st.QueueLen,
		Utilization: st.Utilization,
		Algorithm:   st.Algorithm,
		Effective:   st.Effective,
		Aging:       st.Aging,
		Users:       users,
		Paths:       map[string]PathPoint{},
	}
	if elapsed > 0 {
		p.RPS = float64(len(w.lat)) / elapsed
	}
	p.P50Ms, p.P95Ms, p.P99Ms, p.AvgMs = summarize(w.lat)
	_, p.QueueWaitP95, _, _ = summarize(w.qwait)

	for _, b := range st.Backends {
		p.Backends = append(p.Backends, BackendPoint{
			Name: b.Name, Healthy: b.Healthy, Ejected: b.Ejected, Draining: b.Draining, Down: b.FaultForS > 0,
			Requests: w.byBackend[b.Name], Inflight: b.Inflight, Capacity: b.Capacity, Cores: b.Cores, DBPool: b.DBPool,
			CPUBusy: b.CPUBusy, CPUWaiting: b.CPUWaiting, DBBusy: b.DBBusy, DBWaiting: b.DBWaiting,
		})
	}
	for path, pw := range w.byPath {
		p50, p95, p99, _ := summarize(pw.lat)
		work, _, _, _ := summarize(pw.work)
		p.Paths[path] = PathPoint{
			Requests: len(pw.lat), P50Ms: p50, P95Ms: p95, P99Ms: p99,
			EstMs: est[path].EstMs, WorkMs: work, Queued: st.QueueByPath[path],
		}
	}
	for path, n := range st.QueueByPath { // queued but nothing finished this second
		if _, ok := p.Paths[path]; !ok && len(p.Paths) < maxPathsPerPoint {
			p.Paths[path] = PathPoint{EstMs: est[path].EstMs, Queued: n}
		}
	}

	c.mu.Lock()
	c.history = append(c.history, p)
	if len(c.history) > c.keep {
		c.history = slices.Delete(c.history, 0, len(c.history)-c.keep)
	}
	for ch := range c.subs {
		select {
		case ch <- p:
		default: // slow client: drop this point rather than block
		}
	}
	c.mu.Unlock()
	return p
}

// History returns up to the last n points (all if n <= 0).
func (c *Collector) History(n int) []Point {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.history
	if n > 0 && n < len(h) {
		h = h[len(h)-n:]
	}
	return slices.Clone(h)
}

func (c *Collector) Subscribe() (<-chan Point, func()) {
	ch := make(chan Point, 16)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

// summarize returns p50, p95, p99 and mean (nearest-rank percentiles).
func summarize(xs []float64) (p50, p95, p99, avg float64) {
	if len(xs) == 0 {
		return
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return quantile(s, 0.50), quantile(s, 0.95), quantile(s, 0.99), sum / float64(len(s))
}

// quantile = nearest-rank percentile of an already sorted slice.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted))+0.999999) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}
