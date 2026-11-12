package lb

import (
	"sync"
	"time"
)

// Estimator learns how long each path takes to serve (an EWMA of observed
// service time). SJF uses it as the "job length"; auto mode uses the spread
// between paths to decide whether SJF is worth it.
type Estimator struct {
	mu        sync.Mutex
	paths     map[string]*pathEst
	alpha     float64
	defaultMs float64
	maxPaths  int
}

type pathEst struct {
	ewmaMs float64
	n      uint64
	last   time.Time
}

type PathEstimate struct {
	EstMs   float64 `json:"est_ms"`
	Samples uint64  `json:"samples"`
}

func NewEstimator() *Estimator {
	return &Estimator{paths: map[string]*pathEst{}, alpha: 0.2, defaultMs: 50, maxPaths: 256}
}

func (e *Estimator) Observe(path string, d time.Duration) {
	ms := float64(d) / 1e6
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.paths[path]
	if p == nil {
		if len(e.paths) >= e.maxPaths {
			return // don't let random URLs grow the map forever
		}
		p = &pathEst{ewmaMs: ms}
		e.paths[path] = p
	} else {
		p.ewmaMs = e.alpha*ms + (1-e.alpha)*p.ewmaMs
	}
	p.n++
	p.last = time.Now()
}

// Seed sets an initial estimate (useful before any traffic has been seen).
func (e *Estimator) Seed(path string, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paths[path] = &pathEst{ewmaMs: float64(d) / 1e6, last: time.Now()}
}

// Estimate returns the expected service time in ms. Unknown paths get the
// average of known paths, so a new URL is neither starved nor favoured.
func (e *Estimator) Estimate(path string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.paths[path]; p != nil {
		return p.ewmaMs
	}
	if len(e.paths) == 0 {
		return e.defaultMs
	}
	var sum float64
	for _, p := range e.paths {
		sum += p.ewmaMs
	}
	return sum / float64(len(e.paths))
}

// Spread is max/min estimated cost among paths seen within window.
// 1 means all jobs look the same size (SJF gains nothing over FCFS).
func (e *Estimator) Spread(window time.Duration) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-window)
	lo, hi := 0.0, 0.0
	for _, p := range e.paths {
		if p.last.Before(cutoff) || p.ewmaMs <= 0 {
			continue
		}
		if lo == 0 || p.ewmaMs < lo {
			lo = p.ewmaMs
		}
		hi = max(hi, p.ewmaMs)
	}
	if lo == 0 {
		return 1
	}
	return hi / lo
}

func (e *Estimator) Snapshot() map[string]PathEstimate {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]PathEstimate, len(e.paths))
	for k, p := range e.paths {
		out[k] = PathEstimate{EstMs: p.ewmaMs, Samples: p.n}
	}
	return out
}
