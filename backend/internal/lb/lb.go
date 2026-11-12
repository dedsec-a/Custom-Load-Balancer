// Package lb is an L7 (HTTP) load balancer with switchable scheduling
// (round robin, FCFS, SJF, auto), live backend resource control, a built-in
// load generator and per-second metrics for a control-panel front-end.
package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"loadbalancer/internal/backend"
)

type BackendSpec struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Config struct {
	Backends     []BackendSpec
	Algorithm    Algorithm
	Aging        float64
	MaxQueue     int
	QueueTimeout time.Duration
	LoadTarget   string // URL the load generator hits (normally this LB's own address)
	History      int    // seconds of metrics to keep
	Limits       Limits

	UpstreamTimeout time.Duration // max wait for a backend's response headers (default 15s)
	TrustXFF        bool          // behind a trusted proxy (Caddy): rate-limit by X-Forwarded-For
	AdminRate       float64       // admin writes per second per IP (0 = unlimited)
	PublicRate      float64       // proxied requests per second per IP (0 = unlimited; own load generator exempt)
	ResultsDir      string        // where finished experiments are saved as JSON ("" = memory only)
}

// Passive health check: this many failed real requests in a row eject a
// backend for ejectCooldown; after that, the next good active check brings it back.
const (
	passiveThreshold = 3
	ejectCooldown    = 10 * time.Second
	maxTries         = 2 // first attempt + one retry on another backend
)

// Limits restrict what the admin API accepts. Used for a public demo, so a
// stranger can't overload a small server or make it call arbitrary URLs.
type Limits struct {
	MaxUsers       int           `json:"max_users"`     // 0 = LoadGen's own maximum
	MaxCores       int           `json:"max_cores"`     // 0 = backend maximum
	MaxDBPool      int           `json:"max_db_pool"`   // 0 = backend maximum
	LockBackends   bool          `json:"lock_backends"` // forbid add/remove (blocks SSRF via arbitrary backend URLs)
	IdleStop       time.Duration `json:"-"`             // stop virtual users after this long without an admin change
	IdleStopS      int           `json:"idle_stop_s"`
	MaxFaultS      int           `json:"max_fault_s"`      // longest simulated outage allowed (0 = backend maximum)
	MaxExperimentS int           `json:"max_experiment_s"` // longest experiment allowed (0 = no limit)
}

type LB struct {
	Bal     *Balancer
	Est     *Estimator
	Metrics *Collector
	Load    *LoadGen
	Limits  Limits

	transport *http.Transport
	ctrl      *http.Client // health polls + resource changes
	lastWrite atomic.Int64 // unix ns of the last successful admin change

	trustXFF     bool
	adminLimiter *rateLimiter
	publicLimit  *rateLimiter
	retries      *retryBudget

	runCtx context.Context // lives as long as Run; experiments stop with it
	exps   experiments
	prom   *promCounters
}

func New(cfg Config) (*LB, error) {
	if cfg.History <= 0 {
		cfg.History = 600
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 15 * time.Second
	}
	est := NewEstimator()
	cfg.Limits.IdleStopS = int(cfg.Limits.IdleStop.Seconds())
	l := &LB{
		Est:    est,
		Limits: cfg.Limits,
		Bal: NewBalancer(est, BalancerOptions{
			Algorithm: cfg.Algorithm, Aging: cfg.Aging,
			MaxQueue: cfg.MaxQueue, QueueTimeout: cfg.QueueTimeout,
		}),
		Metrics: NewCollector(cfg.History),
		Load:    NewLoadGen(cfg.LoadTarget),
		transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: cfg.UpstreamTimeout,
			MaxIdleConns:          4096,
			MaxIdleConnsPerHost:   1024,
			IdleConnTimeout:       90 * time.Second,
		},
		ctrl:         &http.Client{Timeout: 2 * time.Second},
		trustXFF:     cfg.TrustXFF,
		adminLimiter: newRateLimiter(cfg.AdminRate, max(cfg.AdminRate*4, 10)),
		publicLimit:  newRateLimiter(cfg.PublicRate, cfg.PublicRate*2),
		retries:      newRetryBudget(0.2, 20), // retries <= ~20% of traffic
		runCtx:       context.Background(),
		exps:         experiments{dir: cfg.ResultsDir},
	}
	l.prom = l.Metrics.prom
	l.exps.load()
	for _, s := range cfg.Backends {
		if err := l.AddBackend(s); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *LB) AddBackend(s BackendSpec) error {
	b, err := newBackend(s.Name, s.URL, l.transport)
	if err != nil {
		return err
	}
	return l.Bal.Add(b)
}

// Run starts the background loops (health polling, auto mode, metrics,
// load generator) and blocks until ctx is cancelled.
func (l *LB) Run(ctx context.Context) {
	l.exps.mu.Lock()
	l.runCtx = ctx // guarded: StartExperiment reads it under the same lock
	l.exps.mu.Unlock()
	l.pollAll(ctx) // know who is healthy before serving
	l.Load.Start(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.pollAll(ctx)
			l.Bal.AutoTick()
			l.Metrics.Flush(now, l.Bal.Snapshot(), l.Est.Snapshot(), l.Load.Users())
			l.idleStop(now)
		}
	}
}

func (l *LB) touched() { l.lastWrite.Store(time.Now().UnixNano()) }

// idleStop turns virtual users off if nobody has changed anything for a
// while, so a forgotten demo tab doesn't burn CPU (and money) forever.
func (l *LB) idleStop(now time.Time) {
	if l.Limits.IdleStop <= 0 || l.Load.Users() == 0 || l.ExperimentRunning() {
		return
	}
	if last := l.lastWrite.Load(); last != 0 && now.Sub(time.Unix(0, last)) < l.Limits.IdleStop {
		return
	}
	cfg := l.Load.Config()
	cfg.Users = 0
	if err := l.Load.Set(cfg); err == nil {
		slog.Info("idle stop: virtual users stopped", "idle_for", l.Limits.IdleStop.String())
	}
}

// ---- proxy (the data plane) ----------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (l *LB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path := r.URL.Path

	if l.publicLimit != nil && !fromLoopbackDirect(r) && !l.publicLimit.allow(clientIP(r, l.trustXFF)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		l.Metrics.Record(path, "", time.Since(start), 0, 0, OutcomeRateLimited)
		return
	}

	b, wait, err := l.Bal.Acquire(r.Context(), path)
	if err != nil {
		status, outcome := http.StatusServiceUnavailable, OutcomeNoBackend
		switch {
		case errors.Is(err, context.Canceled):
			status, outcome = 499, OutcomeClientGone // client went away (nginx convention)
		case errors.Is(err, ErrQueueFull):
			outcome = OutcomeQueueFull // load shedding: the bounded queue is full
		case errors.Is(err, ErrQueueTimeout):
			outcome = OutcomeQueueTimeout
		}
		if status != 499 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, err.Error(), status)
		}
		l.Metrics.Record(path, "", time.Since(start), wait, 0, outcome)
		return
	}
	l.retries.deposit()

	w.Header().Set("X-LB-Queue-Ms", fmt.Sprintf("%.1f", float64(wait)/1e6))
	rec := &statusRecorder{ResponseWriter: w}
	for try := 1; ; try++ {
		w.Header().Set("X-LB-Backend", b.Name)
		a := &attempt{}
		b.proxy.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), attemptKey{}, a)))
		if a.err == nil { // a response went to the client
			if rec.status >= 500 {
				l.passiveFail(b)
			} else {
				b.passiveFails.Store(0)
			}
			b.served.Add(1)
			l.Bal.Release(b)
			break
		}

		// The attempt failed before anything was written to the client.
		failed := b
		l.Bal.Release(failed)
		if r.Context().Err() != nil {
			l.Metrics.Record(path, failed.Name, time.Since(start), wait, 0, OutcomeClientGone)
			return
		}
		l.passiveFail(failed)
		if try >= maxTries || !retryable(r) || !l.retries.withdraw() {
			l.failRequest(w, a.err, path, failed.Name, start, wait)
			return
		}
		next, err := l.Bal.AcquireRetry(failed)
		if err != nil {
			l.failRequest(w, a.err, path, failed.Name, start, wait)
			return
		}
		l.Metrics.Retry()
		b = next
	}

	total := time.Since(start)
	var cost time.Duration
	if rec.status < 400 {
		var ok bool
		cost, ok = serverTiming(w.Header().Get("Server-Timing"), "work")
		if !ok {
			cost = total - wait // fallback: service time excluding LB queueing
		}
		l.Est.Observe(path, cost)
	}
	outcome := OutcomeOK
	if rec.status >= 500 {
		outcome = OutcomeBackendError
	}
	l.Metrics.Record(path, b.Name, total, wait, cost, outcome)
}

// failRequest answers a request whose backend attempts all failed.
func (l *LB) failRequest(w http.ResponseWriter, err error, path, backendName string, start time.Time, wait time.Duration) {
	status := http.StatusBadGateway
	var up *upstreamStatusError
	switch {
	case errors.As(err, &up):
		status = up.status
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		status = http.StatusGatewayTimeout
	}
	http.Error(w, "upstream failed: "+err.Error(), status)
	l.Metrics.Record(path, backendName, time.Since(start), wait, 0, OutcomeBackendError)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// passiveFail counts a failed real request; enough in a row eject the
// backend at once, without waiting for the next active health check.
func (l *LB) passiveFail(b *Backend) {
	if b.passiveFails.Add(1) < passiveThreshold || b.ejectedFor() > 0 {
		return
	}
	b.passiveFails.Store(0)
	b.ejectedUntil.Store(time.Now().Add(ejectCooldown).UnixNano())
	b.ejections.Add(1)
	if b.healthy.Swap(false) {
		slog.Warn("backend ejected", "backend", b.Name, "reason", "passive", "failures", passiveThreshold, "cooldown", ejectCooldown.String())
	}
}

// serverTiming extracts the dur (ms) of metric name from a Server-Timing
// header such as "work;dur=12.5, wait;dur=3".
func serverTiming(h, name string) (time.Duration, bool) {
	for metric := range strings.SplitSeq(h, ",") {
		parts := strings.Split(strings.TrimSpace(metric), ";")
		if parts[0] != name {
			continue
		}
		for _, p := range parts[1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "dur="); ok {
				ms, err := strconv.ParseFloat(v, 64)
				if err != nil || ms < 0 {
					return 0, false
				}
				return time.Duration(ms * 1e6), true
			}
		}
	}
	return 0, false
}

// ---- health / stats polling ----------------------------------------------

func (l *LB) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range l.Bal.Backends() {
		wg.Go(func() { l.poll(ctx, b) })
	}
	wg.Wait()
	l.Bal.Wake() // healthy set or capacities may have changed
}

func (l *LB) poll(ctx context.Context, b *Backend) {
	ctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	st, err := l.fetchStats(ctx, b)
	if err != nil {
		if b.fails.Add(1) >= 2 && b.healthy.Swap(false) {
			slog.Warn("backend down", "backend", b.Name, "reason", "active", "error", err.Error())
		}
		return
	}
	b.stats.Store(st)
	b.fails.Store(0)
	if b.ejectedFor() > 0 {
		return // still cooling down after a passive ejection
	}
	if !b.healthy.Swap(true) {
		b.passiveFails.Store(0)
		slog.Info("backend healthy", "backend", b.Name)
	}
}

func (l *LB) fetchStats(ctx context.Context, b *Backend) (*backend.Stats, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath("stats").String(), nil)
	resp, err := l.ctrl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats: %s", resp.Status)
	}
	var st backend.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SetFault starts (downFor > 0) or ends (0) a simulated outage on a backend.
func (l *LB) SetFault(ctx context.Context, b *Backend, downFor int) error {
	body, _ := json.Marshal(backend.Fault{DownForS: downFor})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, b.URL.JoinPath("admin", "fault").String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.ctrl.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s %s", b.Name, resp.Status, e.Error)
	}
	if downFor > 0 {
		b.faultUntil.Store(time.Now().Add(time.Duration(downFor) * time.Second).UnixNano())
	} else {
		b.faultUntil.Store(0)
		b.ejectedUntil.Store(0) // recovered on purpose: let the next check bring it back
	}
	return nil
}

// SetResources changes a backend's simulated cores / DB pool.
func (l *LB) SetResources(ctx context.Context, b *Backend, res backend.Resources) (*backend.Stats, error) {
	body, _ := json.Marshal(res)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, b.URL.JoinPath("admin", "resources").String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.ctrl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("%s: %s %s", b.Name, resp.Status, e.Error)
	}
	var st backend.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	b.stats.Store(&st)
	l.Bal.Wake() // capacity may have grown: let queued requests through
	return &st, nil
}
