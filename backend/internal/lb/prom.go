package lb

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prometheus metrics in the text exposition format (version 0.0.4), written
// by hand: a handful of counters, gauges and two histograms don't justify a
// client library.

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type histogram struct {
	counts []uint64 // per bucket (not cumulative); last = +Inf
	sum    float64
	n      uint64
}

func newHistogram() *histogram { return &histogram{counts: make([]uint64, len(latencyBuckets)+1)} }

func (h *histogram) observe(seconds float64) {
	i, _ := slices.BinarySearch(latencyBuckets, seconds)
	h.counts[i]++
	h.sum += seconds
	h.n++
}

type promCounters struct {
	mu        sync.Mutex
	requests  map[[2]string]uint64 // {path, outcome} -> count
	latency   map[string]*histogram
	queueWait *histogram
	retries   uint64
}

func newPromCounters() *promCounters {
	return &promCounters{requests: map[[2]string]uint64{}, latency: map[string]*histogram{}, queueWait: newHistogram()}
}

var outcomeNames = map[Outcome]string{
	OutcomeOK: "ok", OutcomeBackendError: "backend_error", OutcomeNoBackend: "no_backend",
	OutcomeQueueFull: "shed_queue_full", OutcomeQueueTimeout: "shed_queue_timeout",
	OutcomeRateLimited: "rate_limited", OutcomeClientGone: "client_gone",
}

func (p *promCounters) record(path string, latency, wait time.Duration, o Outcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.latency) >= maxPathsPerPoint {
		if _, ok := p.latency[path]; !ok {
			path = "(other)" // bound label cardinality
		}
	}
	p.requests[[2]string{path, outcomeNames[o]}]++
	if o == OutcomeOK {
		h := p.latency[path]
		if h == nil {
			h = newHistogram()
			p.latency[path] = h
		}
		h.observe(latency.Seconds())
		p.queueWait.observe(wait.Seconds())
	}
}

func (p *promCounters) retry() {
	p.mu.Lock()
	p.retries++
	p.mu.Unlock()
}

// label escapes a label value.
func label(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func writeHistogram(w io.Writer, name, labels string, h *histogram) {
	sep := ""
	if labels != "" {
		sep = ","
	}
	var cum uint64
	for i, le := range latencyBuckets {
		cum += h.counts[i]
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"%s\"} %d\n", name, labels, sep, num(le), cum)
	}
	cum += h.counts[len(latencyBuckets)]
	fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, labels, sep, cum)
	if labels != "" {
		labels = "{" + labels + "}"
	}
	fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", name, labels, num(h.sum), name, labels, h.n)
}

// ServeMetrics writes every metric. Mounted at GET /metrics on the admin port.
func (l *LB) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := l.prom
	st := l.Bal.Snapshot()

	p.mu.Lock()
	fmt.Fprintln(w, "# HELP lb_requests_total Requests handled by the load balancer, by path and outcome.")
	fmt.Fprintln(w, "# TYPE lb_requests_total counter")
	keys := make([][2]string, 0, len(p.requests))
	for k := range p.requests {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
	for _, k := range keys {
		fmt.Fprintf(w, "lb_requests_total{path=\"%s\",outcome=\"%s\"} %d\n", label(k[0]), k[1], p.requests[k])
	}
	fmt.Fprintln(w, "# HELP lb_request_duration_seconds End-to-end latency of successful requests, including LB queueing.")
	fmt.Fprintln(w, "# TYPE lb_request_duration_seconds histogram")
	paths := make([]string, 0, len(p.latency))
	for k := range p.latency {
		paths = append(paths, k)
	}
	slices.Sort(paths)
	for _, path := range paths {
		writeHistogram(w, "lb_request_duration_seconds", fmt.Sprintf("path=\"%s\"", label(path)), p.latency[path])
	}
	fmt.Fprintln(w, "# HELP lb_queue_wait_seconds Time successful requests spent in the LB queue.")
	fmt.Fprintln(w, "# TYPE lb_queue_wait_seconds histogram")
	writeHistogram(w, "lb_queue_wait_seconds", "", p.queueWait)
	fmt.Fprintln(w, "# HELP lb_retries_total Requests retried on another backend after a failure.")
	fmt.Fprintln(w, "# TYPE lb_retries_total counter")
	fmt.Fprintf(w, "lb_retries_total %d\n", p.retries)
	p.mu.Unlock()

	gauge := func(name, help string, v float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, num(v))
	}
	gauge("lb_queue_length", "Requests waiting in the LB queue.", float64(st.QueueLen))
	gauge("lb_utilization", "(In flight + queued) / total slots across healthy backends.", st.Utilization)
	gauge("lb_virtual_users", "Closed-loop virtual users running.", float64(l.Load.Users()))
	running := 0.0
	if l.ExperimentRunning() {
		running = 1
	}
	gauge("lb_experiment_running", "1 while a controlled experiment is running.", running)
	fmt.Fprintln(w, "# HELP lb_algorithm_info The configured and the effective scheduling algorithm (value is always 1).")
	fmt.Fprintln(w, "# TYPE lb_algorithm_info gauge")
	fmt.Fprintf(w, "lb_algorithm_info{configured=\"%s\",effective=\"%s\"} 1\n", st.Algorithm, st.Effective)

	perBackend := []struct {
		name, help, typ string
		v               func(BackendView) float64
	}{
		{"lb_backend_up", "1 if the backend is receiving traffic.", "gauge", func(b BackendView) float64 { return boolf(b.Healthy && !b.Draining) }},
		{"lb_backend_inflight", "Requests in flight on the backend.", "gauge", func(b BackendView) float64 { return float64(b.Inflight) }},
		{"lb_backend_capacity", "LB slots for the backend.", "gauge", func(b BackendView) float64 { return float64(b.Capacity) }},
		{"lb_backend_cores", "Simulated CPU cores.", "gauge", func(b BackendView) float64 { return float64(b.Cores) }},
		{"lb_backend_db_pool", "Simulated DB connections.", "gauge", func(b BackendView) float64 { return float64(b.DBPool) }},
		{"lb_backend_served_total", "Responses served by the backend.", "counter", func(b BackendView) float64 { return float64(b.Served) }},
		{"lb_backend_ejections_total", "Times passive health checks ejected the backend.", "counter", func(b BackendView) float64 { return float64(b.Ejections) }},
	}
	for _, m := range perBackend {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, b := range st.Backends {
			fmt.Fprintf(w, "%s{backend=\"%s\"} %s\n", m.name, label(b.Name), num(m.v(b)))
		}
	}
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
