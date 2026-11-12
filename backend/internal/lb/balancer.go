package lb

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Algorithm is how the balancer picks which request runs next and where.
type Algorithm string

const (
	// RoundRobin forwards every request immediately to the next healthy
	// backend in turn. No LB-side queue; backends queue internally.
	RoundRobin Algorithm = "round_robin"
	// FCFS holds requests in an LB queue when every backend is at capacity
	// and releases them oldest-first to the least-loaded backend.
	FCFS Algorithm = "fcfs"
	// SJF is like FCFS but releases the request with the shortest expected
	// service time first (with aging so long jobs can't starve forever).
	SJF Algorithm = "sjf"
	// LeastConn forwards every request immediately to the backend with the
	// fewest requests in flight relative to its slots (weighted least
	// connections, the common industry default). No LB-side queue.
	LeastConn Algorithm = "least_conn"
	// Auto picks round robin, FCFS or SJF every second based on traffic.
	Auto Algorithm = "auto"
)

func ParseAlgorithm(s string) (Algorithm, error) {
	switch a := Algorithm(s); a {
	case RoundRobin, LeastConn, FCFS, SJF, Auto:
		return a, nil
	}
	return "", fmt.Errorf("unknown algorithm %q (want round_robin, least_conn, fcfs, sjf or auto)", s)
}

// immediate reports whether an algorithm forwards at once (no LB queue).
func (a Algorithm) immediate() bool { return a == RoundRobin || a == LeastConn }

var (
	ErrNoBackends   = errors.New("no healthy backends")
	ErrQueueFull    = errors.New("load balancer queue is full")
	ErrQueueTimeout = errors.New("timed out waiting in load balancer queue")
)

// Auto-mode thresholds. Utilization = (in-flight + queued) / total capacity.
const (
	autoQueueAbove = 0.9 // switch RR -> queued algorithm when util goes above this
	autoRRBelow    = 0.6 // switch back to RR when util drops below this and queue is empty
	autoSJFSpread  = 2.0 // use SJF when slowest/fastest path cost ratio >= this
	autoFCFSSpread = 1.5 // fall back to FCFS when it drops below this
)

type Balancer struct {
	mu           sync.Mutex
	backends     []*Backend
	algo         Algorithm // what the user chose
	effective    Algorithm // what is actually running (differs only in auto)
	rr           int
	queue        waitQueue
	seq          uint64
	aging        float64 // SJF: ms of cost forgiven per ms already waited
	maxQueue     int
	queueTimeout time.Duration
	epoch        time.Time
	est          *Estimator
	switches     uint64
	lastSwitch   time.Time
	lastReason   string
}

type BalancerOptions struct {
	Algorithm    Algorithm
	Aging        float64
	MaxQueue     int
	QueueTimeout time.Duration
}

func NewBalancer(est *Estimator, o BalancerOptions) *Balancer {
	if o.Algorithm == "" {
		o.Algorithm = Auto
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = 10000
	}
	if o.QueueTimeout <= 0 {
		o.QueueTimeout = 10 * time.Second
	}
	b := &Balancer{
		algo:         o.Algorithm,
		effective:    o.Algorithm,
		aging:        o.Aging,
		maxQueue:     o.MaxQueue,
		queueTimeout: o.QueueTimeout,
		epoch:        time.Now(),
		est:          est,
	}
	if b.effective == Auto {
		b.effective = RoundRobin
		b.lastReason = "start: low load"
	}
	return b
}

// ---- request path --------------------------------------------------------

// Acquire picks a backend for a request to path, waiting in the LB queue if
// needed. The caller must call Release(b) when the request finishes.
func (bl *Balancer) Acquire(ctx context.Context, path string) (*Backend, time.Duration, error) {
	start := time.Now()
	bl.mu.Lock()
	if !bl.anyHealthyLocked() {
		bl.mu.Unlock()
		return nil, 0, ErrNoBackends
	}
	if bl.effective.immediate() {
		b := bl.pickImmediateLocked(nil)
		b.inflight++
		bl.mu.Unlock()
		return b, 0, nil
	}
	// Queued algorithms: go straight through only if nobody is waiting
	// (otherwise we'd jump the queue).
	if bl.queue.Len() == 0 {
		if b := bl.pickLeastLoadedLocked(); b != nil {
			b.inflight++
			bl.mu.Unlock()
			return b, 0, nil
		}
	}
	if bl.queue.Len() >= bl.maxQueue {
		bl.mu.Unlock()
		return nil, 0, ErrQueueFull
	}
	w := &waiter{
		path:  path,
		cost:  bl.est.Estimate(path),
		enq:   start,
		seq:   bl.seq,
		ready: make(chan *Backend, 1),
	}
	bl.seq++
	w.key = bl.keyLocked(w)
	heap.Push(&bl.queue, w)
	timeout := bl.queueTimeout
	bl.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case b := <-w.ready:
		return b, time.Since(start), nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = ErrQueueTimeout
	}

	bl.mu.Lock()
	if w.index >= 0 { // still queued: just leave
		heap.Remove(&bl.queue, w.index)
		bl.mu.Unlock()
		return nil, time.Since(start), err
	}
	bl.mu.Unlock()
	// We were assigned a backend at the same moment we gave up: hand it back.
	bl.Release(<-w.ready)
	return nil, time.Since(start), err
}

// AcquireRetry picks a backend other than avoid for a retry, immediately
// and without queueing: the request already waited once. Weighted least
// connections, ignoring slot limits.
func (bl *Balancer) AcquireRetry(avoid *Backend) (*Backend, error) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	b := bl.pickLeastConnLocked(avoid)
	if b == nil {
		return nil, ErrNoBackends
	}
	b.inflight++
	return b, nil
}

// Release frees the backend slot and lets the next queued request run.
func (bl *Balancer) Release(b *Backend) {
	bl.mu.Lock()
	b.inflight--
	if b.draining.Load() && b.inflight == 0 {
		bl.removeLocked(b)
	}
	bl.dispatchLocked()
	bl.mu.Unlock()
}

// dispatchLocked hands queued requests to backends while there is room.
func (bl *Balancer) dispatchLocked() {
	for bl.queue.Len() > 0 {
		var b *Backend
		if bl.effective.immediate() {
			b = bl.pickImmediateLocked(nil)
		} else {
			b = bl.pickLeastLoadedLocked()
		}
		if b == nil {
			return
		}
		w := heap.Pop(&bl.queue).(*waiter)
		b.inflight++
		w.ready <- b
	}
}

func (bl *Balancer) anyHealthyLocked() bool {
	for _, b := range bl.backends {
		if b.usable() {
			return true
		}
	}
	return false
}

func (bl *Balancer) pickImmediateLocked(avoid *Backend) *Backend {
	if bl.effective == LeastConn {
		return bl.pickLeastConnLocked(avoid)
	}
	return bl.pickRRLocked()
}

func (bl *Balancer) pickRRLocked() *Backend {
	n := len(bl.backends)
	for i := range n {
		idx := (bl.rr + i) % n
		if b := bl.backends[idx]; b.usable() {
			bl.rr = (idx + 1) % n
			return b
		}
	}
	return nil
}

// pickLeastConnLocked returns the usable backend (other than avoid) with
// the lowest in-flight/capacity ratio, ignoring slot limits. Ties rotate,
// so an idle cluster doesn't send everything to the first backend.
func (bl *Balancer) pickLeastConnLocked(avoid *Backend) *Backend {
	n := len(bl.backends)
	var best *Backend
	bestLoad, bestIdx := 0.0, 0
	for i := range n {
		idx := (bl.rr + i) % n
		b := bl.backends[idx]
		if !b.usable() || b == avoid {
			continue
		}
		load := float64(b.inflight) / float64(b.capacity())
		if best == nil || load < bestLoad {
			best, bestLoad, bestIdx = b, load, idx
		}
	}
	if best != nil {
		bl.rr = (bestIdx + 1) % n
	}
	return best
}

// pickLeastLoadedLocked returns the healthy backend with the lowest
// in-flight/capacity ratio that still has room, or nil if all are full.
func (bl *Balancer) pickLeastLoadedLocked() *Backend {
	var best *Backend
	bestLoad := 0.0
	for _, b := range bl.backends {
		if !b.usable() {
			continue
		}
		c := b.capacity()
		if b.inflight >= c {
			continue
		}
		load := float64(b.inflight) / float64(c)
		if best == nil || load < bestLoad {
			best, bestLoad = b, load
		}
	}
	return best
}

// keyLocked is the queue priority (lower runs first).
//
//	FCFS: arrival time.
//	SJF:  expected cost + aging * arrival time. With aging = 0 this is pure
//	      SJF; with aging = 0.1 a job that arrived 1 s earlier is treated as
//	      100 ms "shorter", which bounds how long a long job can be starved.
func (bl *Balancer) keyLocked(w *waiter) float64 {
	arrivalMs := float64(w.enq.Sub(bl.epoch)) / 1e6
	if bl.effective == SJF {
		return w.cost + bl.aging*arrivalMs
	}
	return arrivalMs
}

// setEffectiveLocked switches the running algorithm and re-sorts the queue.
func (bl *Balancer) setEffectiveLocked(a Algorithm, reason string) {
	bl.lastReason = reason
	if a == bl.effective {
		return
	}
	bl.effective = a
	bl.switches++
	bl.lastSwitch = time.Now()
	for _, w := range bl.queue {
		w.key = bl.keyLocked(w)
	}
	heap.Init(&bl.queue)
	bl.dispatchLocked()
}

// ---- control -------------------------------------------------------------

func (bl *Balancer) SetAlgorithm(a Algorithm) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.algo = a
	if a == Auto {
		bl.autoLocked()
		return
	}
	bl.setEffectiveLocked(a, "set by user")
}

func (bl *Balancer) SetAging(v float64) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	bl.aging = v
	for _, w := range bl.queue {
		w.key = bl.keyLocked(w)
	}
	heap.Init(&bl.queue)
}

func (bl *Balancer) SetQueueLimits(maxQueue int, timeout time.Duration) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if maxQueue > 0 {
		bl.maxQueue = maxQueue
	}
	if timeout > 0 {
		bl.queueTimeout = timeout
	}
}

// AutoTick re-evaluates the algorithm when in auto mode. Call ~1/s.
func (bl *Balancer) AutoTick() {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	if bl.algo == Auto {
		bl.autoLocked()
	}
}

func (bl *Balancer) autoLocked() {
	if bl.effective == LeastConn { // auto chooses among RR, FCFS and SJF
		bl.setEffectiveLocked(RoundRobin, "auto starts from round robin")
	}
	util := bl.utilizationLocked()
	spread := bl.est.Spread(10 * time.Second)
	queued := !bl.effective.immediate()

	pickQueued := func() Algorithm {
		switch {
		case spread >= autoSJFSpread:
			return SJF
		case spread < autoFCFSSpread:
			return FCFS
		case bl.effective == SJF || bl.effective == FCFS:
			return bl.effective // in the hysteresis band: keep current
		default:
			return FCFS
		}
	}

	switch {
	case !queued && util >= autoQueueAbove:
		a := pickQueued()
		bl.setEffectiveLocked(a, fmt.Sprintf("utilization %.0f%% >= %.0f%%, job-size spread %.1fx", util*100, autoQueueAbove*100, spread))
	case queued && util < autoRRBelow && bl.queue.Len() == 0:
		bl.setEffectiveLocked(RoundRobin, fmt.Sprintf("utilization %.0f%% < %.0f%%, queue empty", util*100, autoRRBelow*100))
	case queued && pickQueued() != bl.effective:
		bl.setEffectiveLocked(pickQueued(), fmt.Sprintf("job-size spread %.1fx", spread))
	default:
		// No switch: still explain the current choice, so the UI never shows
		// a stale reason like "set by user" while in auto mode.
		bl.lastReason = fmt.Sprintf("keeping %s: utilization %.0f%%, job-size spread %.1fx", bl.effective, util*100, spread)
	}
}

func (bl *Balancer) utilizationLocked() float64 {
	load, capacity := bl.queue.Len(), 0
	for _, b := range bl.backends {
		if b.usable() {
			load += b.inflight
			capacity += b.capacity()
		}
	}
	if capacity == 0 {
		return 0
	}
	return float64(load) / float64(capacity)
}

// ---- backend set ---------------------------------------------------------

func (bl *Balancer) Add(b *Backend) error {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	for _, x := range bl.backends {
		if x.Name == b.Name {
			return fmt.Errorf("backend %q already exists", b.Name)
		}
	}
	bl.backends = append(bl.backends, b)
	bl.dispatchLocked()
	return nil
}

// Drain stops sending new requests to a backend and removes it once its
// in-flight requests finish (immediately if it has none). Graceful removal:
// nothing already running is cut off.
func (bl *Balancer) Drain(name string) bool {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	i := slices.IndexFunc(bl.backends, func(b *Backend) bool { return b.Name == name })
	if i < 0 {
		return false
	}
	b := bl.backends[i]
	b.draining.Store(true)
	if b.inflight == 0 {
		bl.removeLocked(b)
	}
	bl.dispatchLocked()
	return true
}

func (bl *Balancer) removeLocked(b *Backend) {
	i := slices.Index(bl.backends, b)
	if i < 0 {
		return
	}
	bl.backends = slices.Delete(bl.backends, i, i+1)
	if len(bl.backends) > 0 {
		bl.rr %= len(bl.backends)
	} else {
		bl.rr = 0
	}
}

func (bl *Balancer) Get(name string) *Backend {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	for _, b := range bl.backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

func (bl *Balancer) Backends() []*Backend {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return slices.Clone(bl.backends)
}

func (bl *Balancer) SetMaxInflight(b *Backend, n int) {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	b.maxInflight = max(n, 0)
	bl.dispatchLocked()
}

// Wake re-runs dispatch, e.g. after a backend came back or grew capacity.
func (bl *Balancer) Wake() {
	bl.mu.Lock()
	bl.dispatchLocked()
	bl.mu.Unlock()
}

// ---- snapshot ------------------------------------------------------------

type BackendView struct {
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Healthy     bool    `json:"healthy"`
	Inflight    int     `json:"inflight"`
	Capacity    int     `json:"capacity"`
	MaxInflight int     `json:"max_inflight"` // 0 = auto
	Served      uint64  `json:"served"`
	ProxyErrors uint64  `json:"proxy_errors"`
	Draining    bool    `json:"draining"`
	Ejected     bool    `json:"ejected"`       // removed by passive checks, cooling down
	EjectedForS float64 `json:"ejected_for_s"` // seconds of cool-down left
	Ejections   uint64  `json:"ejections"`
	FaultForS   float64 `json:"fault_for_s"` // simulated outage time left (set via the LB)
	Cores       int     `json:"cores"`
	DBPool      int     `json:"db_pool"`
	CPUBusy     int     `json:"cpu_busy"`
	CPUWaiting  int     `json:"cpu_waiting"`
	DBBusy      int     `json:"db_busy"`
	DBWaiting   int     `json:"db_waiting"`
}

type BalancerState struct {
	Algorithm      Algorithm      `json:"algorithm"`
	Effective      Algorithm      `json:"effective"`
	Reason         string         `json:"reason"`
	Switches       uint64         `json:"switches"`
	Aging          float64        `json:"aging"`
	QueueLen       int            `json:"queue_len"`
	QueueByPath    map[string]int `json:"queue_by_path"`
	MaxQueue       int            `json:"max_queue"`
	QueueTimeoutMs int64          `json:"queue_timeout_ms"`
	Utilization    float64        `json:"utilization"`
	Backends       []BackendView  `json:"backends"`
}

func (bl *Balancer) Snapshot() BalancerState {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	s := BalancerState{
		Algorithm:      bl.algo,
		Effective:      bl.effective,
		Reason:         bl.lastReason,
		Switches:       bl.switches,
		Aging:          bl.aging,
		QueueLen:       bl.queue.Len(),
		QueueByPath:    map[string]int{},
		MaxQueue:       bl.maxQueue,
		QueueTimeoutMs: bl.queueTimeout.Milliseconds(),
		Utilization:    bl.utilizationLocked(),
		Backends:       make([]BackendView, 0, len(bl.backends)),
	}
	for _, w := range bl.queue {
		s.QueueByPath[w.path]++
	}
	for _, b := range bl.backends {
		v := BackendView{
			Name:        b.Name,
			URL:         b.URL.String(),
			Healthy:     b.healthy.Load(),
			Inflight:    b.inflight,
			Capacity:    b.capacity(),
			MaxInflight: b.maxInflight,
			Served:      b.served.Load(),
			ProxyErrors: b.failed.Load(),
			Draining:    b.draining.Load(),
			Ejections:   b.ejections.Load(),
		}
		if left := b.ejectedFor(); left > 0 {
			v.Ejected, v.EjectedForS = true, left.Seconds()
		}
		if left := b.faultFor(); left > 0 {
			v.FaultForS = left.Seconds()
		}
		if st := b.stats.Load(); st != nil {
			v.Cores, v.DBPool = st.Cores, st.DBPool
			v.CPUBusy, v.CPUWaiting = st.CPU.Busy, st.CPU.Waiting
			v.DBBusy, v.DBWaiting = st.DB.Busy, st.DB.Waiting
		}
		s.Backends = append(s.Backends, v)
	}
	return s
}

// ---- priority queue ------------------------------------------------------

type waiter struct {
	path  string
	cost  float64
	enq   time.Time
	seq   uint64
	key   float64
	index int // position in heap; -1 once popped
	ready chan *Backend
}

type waitQueue []*waiter

func (q waitQueue) Len() int { return len(q) }
func (q waitQueue) Less(i, j int) bool {
	if q[i].key != q[j].key {
		return q[i].key < q[j].key
	}
	return q[i].seq < q[j].seq
}
func (q waitQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}
func (q *waitQueue) Push(x any) {
	w := x.(*waiter)
	w.index = len(*q)
	*q = append(*q, w)
}
func (q *waitQueue) Pop() any {
	old := *q
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	w.index = -1
	*q = old[:n-1]
	return w
}
