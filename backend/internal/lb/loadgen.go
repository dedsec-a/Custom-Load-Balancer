package lb

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxUsers    = 2000
	MaxOpenRate = 5000 // open-loop arrivals per second
	maxOpenBusy = 4000 // open-loop requests in flight before arrivals are dropped
)

// LoadConfig is the traffic to generate.
//
// Closed loop (the default): Users virtual users, each looping "pick a path
// from the mix, request it, think, repeat". Arrivals slow down when the
// system slows down, like real users.
//
// Open loop: requests arrive as a Poisson process at Rate per second no
// matter how slow the system gets. That is what queueing theory (M/M/c)
// assumes, so the theory check uses it.
//
// Seed (non-zero) makes the request sequence reproducible: every virtual
// user draws its paths from its own seeded stream, so two runs with the
// same seed send the same requests in the same order per user.
type LoadConfig struct {
	Users   int            `json:"users"`
	ThinkMs int            `json:"think_ms"`
	Mix     map[string]int `json:"mix"` // path -> weight, e.g. {"/browse":70,"/search":30}
	Seed    uint64         `json:"seed"`
	Mode    string         `json:"mode"` // "closed" (default) | "open"
	Rate    float64        `json:"rate"` // open loop: arrivals per second
}

func (c LoadConfig) open() bool { return c.Mode == "open" }

func (c LoadConfig) Validate() error {
	if c.Users < 0 || c.Users > MaxUsers {
		return fmt.Errorf("users must be 0..%d", MaxUsers)
	}
	if c.ThinkMs < 0 || c.ThinkMs > 60000 {
		return fmt.Errorf("think_ms must be 0..60000")
	}
	if c.Mode != "" && c.Mode != "closed" && c.Mode != "open" {
		return fmt.Errorf("mode must be closed or open")
	}
	if c.Rate < 0 || c.Rate > MaxOpenRate || math.IsNaN(c.Rate) {
		return fmt.Errorf("rate must be 0..%d", MaxOpenRate)
	}
	if len(c.Mix) > 32 {
		return fmt.Errorf("mix can have at most 32 paths")
	}
	total := 0
	for p, w := range c.Mix {
		if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
			return fmt.Errorf("mix path %q must start with a single /", p)
		}
		if w < 0 {
			return fmt.Errorf("mix weight for %q must be >= 0", p)
		}
		total += w
	}
	if len(c.Mix) > 0 && total == 0 {
		return fmt.Errorf("mix weights must not all be 0")
	}
	return nil
}

// LoadGen runs virtual users against the load balancer's own front door,
// so generated traffic goes through exactly the same path as real traffic.
type LoadGen struct {
	target string
	client *http.Client

	mu      sync.RWMutex
	ctx     context.Context
	cfg     LoadConfig
	paths   []string // flattened mix for fast picking
	weights []int
	total   int
	workers []context.CancelFunc
	openRun context.CancelFunc // open-loop dispatcher, if running

	sent    atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64 // open loop: arrivals dropped because too many were in flight
	busy    atomic.Int64  // open loop: requests in flight
}

func NewLoadGen(target string) *LoadGen {
	tr := &http.Transport{
		MaxIdleConns:        MaxUsers,
		MaxIdleConnsPerHost: MaxUsers,
		IdleConnTimeout:     90 * time.Second,
	}
	return &LoadGen{
		target: strings.TrimSuffix(target, "/"),
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
		ctx:    context.Background(),
		cfg:    LoadConfig{Mix: map[string]int{"/browse": 70, "/search": 20, "/checkout": 10}},
	}
}

// Start binds workers to ctx: cancelling it stops all virtual users.
func (g *LoadGen) Start(ctx context.Context) {
	g.mu.Lock()
	g.ctx = ctx
	g.mu.Unlock()
	g.Set(g.Config())
}

func (g *LoadGen) Config() LoadConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	c := g.cfg
	c.Mix = maps.Clone(c.Mix)
	return c
}

// Users is the number of closed-loop virtual users running.
func (g *LoadGen) Users() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.workers)
}

// newRNG returns a seeded stream (reproducible) or a random one (seed 0).
func newRNG(seed, stream uint64) *rand.Rand {
	if seed == 0 {
		return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return rand.New(rand.NewPCG(seed, stream))
}

func (g *LoadGen) Set(c LoadConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.cfg
	g.cfg = c
	g.cfg.Mix = maps.Clone(c.Mix)
	g.paths = slices.Sorted(maps.Keys(c.Mix))
	g.weights = g.weights[:0]
	g.total = 0
	for _, p := range g.paths {
		g.weights = append(g.weights, c.Mix[p])
		g.total += c.Mix[p]
	}

	// A new seed or mode means a new, reproducible run: restart from scratch
	// so every user's stream begins at its first draw.
	if c.Seed != prev.Seed || c.Mode != prev.Mode {
		g.stopAllLocked()
	}
	if g.openRun != nil && (!c.open() || c.Rate != prev.Rate) {
		g.openRun()
		g.openRun = nil
	}

	if c.open() {
		for _, cancel := range g.workers {
			cancel()
		}
		g.workers = nil
		if g.openRun == nil && c.Rate > 0 {
			ctx, cancel := context.WithCancel(g.ctx)
			g.openRun = cancel
			go g.openLoop(ctx, c.Rate, newRNG(c.Seed, math.MaxUint64))
		}
		return nil
	}

	for len(g.workers) < c.Users {
		ctx, cancel := context.WithCancel(g.ctx)
		i := uint64(len(g.workers))
		g.workers = append(g.workers, cancel)
		go g.worker(ctx, newRNG(c.Seed, i))
	}
	for len(g.workers) > c.Users {
		last := len(g.workers) - 1
		g.workers[last]()
		g.workers = g.workers[:last]
	}
	return nil
}

func (g *LoadGen) stopAllLocked() {
	for _, cancel := range g.workers {
		cancel()
	}
	g.workers = nil
	if g.openRun != nil {
		g.openRun()
		g.openRun = nil
	}
}

// pick draws a path from the mix with the caller's own random stream.
func (g *LoadGen) pick(rng *rand.Rand) (string, time.Duration) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	think := time.Duration(g.cfg.ThinkMs) * time.Millisecond
	if g.total == 0 {
		return "/browse", think
	}
	r := rng.IntN(g.total)
	for i, w := range g.weights {
		if r < w {
			return g.paths[i], think
		}
		r -= w
	}
	return g.paths[len(g.paths)-1], think
}

// fire sends one request and returns how long a polite client would pause
// afterwards (0 = go again at once). ok=false means stop (ctx cancelled).
func (g *LoadGen) fire(ctx context.Context, path string) (pause time.Duration, ok bool) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.target+path, nil)
	req.Header.Set("User-Agent", "lb-loadgen")
	resp, err := g.client.Do(req)
	g.sent.Add(1)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false
		}
		g.failed.Add(1)
		return 100 * time.Millisecond, true // don't spin when the LB is down
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		g.failed.Add(1)
		// Behave like a well-mannered client: honour Retry-After (capped),
		// otherwise back off briefly. Shed responses are instant, so without
		// this a virtual user would hammer the LB thousands of times a second.
		backoff := 50 * time.Millisecond
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			backoff = min(time.Duration(s)*time.Second, 2*time.Second)
		}
		return backoff, true
	}
	return 0, true
}

func (g *LoadGen) worker(ctx context.Context, rng *rand.Rand) {
	// Stagger start so 500 users don't fire in the same microsecond.
	if !sleepCtx(ctx, time.Duration(rng.IntN(200))*time.Millisecond) {
		return
	}
	for ctx.Err() == nil {
		path, think := g.pick(rng)
		pause, ok := g.fire(ctx, path)
		if !ok || !sleepCtx(ctx, max(pause, think)) {
			return
		}
	}
}

// openLoop generates Poisson arrivals: exponential gaps with mean 1/rate.
// Arrivals are scheduled against an absolute clock, so a slow send never
// lowers the rate.
func (g *LoadGen) openLoop(ctx context.Context, rate float64, rng *rand.Rand) {
	next := time.Now()
	for {
		next = next.Add(time.Duration(rng.ExpFloat64() / rate * float64(time.Second)))
		if !sleepCtx(ctx, time.Until(next)) {
			return
		}
		path, _ := g.pick(rng)
		if g.busy.Load() >= maxOpenBusy {
			g.dropped.Add(1)
			continue
		}
		g.busy.Add(1)
		go func() {
			defer g.busy.Add(-1)
			g.fire(ctx, path)
		}()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
