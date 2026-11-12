// Package sim simulates a server's limited resources: CPU cores and a DB
// connection pool. Both are resizable at runtime so the control panel can
// "add cores" or "grow the DB pool" while traffic is flowing.
package sim

import (
	"sync"
	"time"
)

// Pool is a resizable FIFO semaphore. Size = how many holders at once
// (cores, or DB connections). Callers beyond that wait in a queue.
type Pool struct {
	mu      sync.Mutex
	size    int
	used    int
	waiters []chan struct{}
	served  uint64
	waitNs  int64 // total time all callers spent waiting
}

// PoolStats is a point-in-time view of a Pool.
type PoolStats struct {
	Size        int     `json:"size"`
	Busy        int     `json:"busy"`
	Waiting     int     `json:"waiting"`
	Served      uint64  `json:"served"`
	TotalWaitMs float64 `json:"total_wait_ms"`
}

func NewPool(size int) *Pool {
	return &Pool{size: max(size, 1)}
}

// Acquire takes one slot, blocking while the pool is full.
// It returns how long the caller waited.
func (p *Pool) Acquire() time.Duration {
	start := time.Now()
	p.mu.Lock()
	if p.used < p.size && len(p.waiters) == 0 {
		p.used++
		p.served++
		p.mu.Unlock()
		return 0
	}
	ch := make(chan struct{})
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()

	<-ch // the releaser already counted us as "used"
	waited := time.Since(start)
	p.mu.Lock()
	p.waitNs += int64(waited)
	p.mu.Unlock()
	return waited
}

// Release gives a slot back and wakes the next waiter, if any.
func (p *Pool) Release() {
	p.mu.Lock()
	p.used--
	p.wakeLocked()
	p.mu.Unlock()
}

// Resize changes the pool size. Growing wakes waiters immediately;
// shrinking takes effect as current holders release.
func (p *Pool) Resize(size int) {
	p.mu.Lock()
	p.size = max(size, 1)
	p.wakeLocked()
	p.mu.Unlock()
}

func (p *Pool) wakeLocked() {
	for p.used < p.size && len(p.waiters) > 0 {
		ch := p.waiters[0]
		p.waiters[0] = nil
		p.waiters = p.waiters[1:]
		p.used++
		p.served++
		close(ch)
	}
}

func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{
		Size:        p.size,
		Busy:        p.used,
		Waiting:     len(p.waiters),
		Served:      p.served,
		TotalWaitMs: float64(p.waitNs) / 1e6,
	}
}

// Sim holds the simulated resources of one backend.
type Sim struct {
	cpu *Pool
	db  *Pool
}

// New creates a simulator with the given number of cores and DB connections.
func New(cores, dbPool int) *Sim {
	return &Sim{cpu: NewPool(cores), db: NewPool(dbPool)}
}

// CPU simulates CPU work that keeps one core busy for d.
// It returns how long the caller waited in the queue for a free core.
func (s *Sim) CPU(d time.Duration) time.Duration {
	waited := s.cpu.Acquire()
	defer s.cpu.Release()
	time.Sleep(d) // the "work": sleeping uses no real CPU
	return waited
}

// DB simulates a query that holds one pooled connection for d.
// It returns how long the caller waited for a free connection.
func (s *Sim) DB(d time.Duration) time.Duration {
	waited := s.db.Acquire()
	defer s.db.Release()
	time.Sleep(d)
	return waited
}

func (s *Sim) SetCores(n int)  { s.cpu.Resize(n) }
func (s *Sim) SetDBPool(n int) { s.db.Resize(n) }

func (s *Sim) CPUStats() PoolStats { return s.cpu.Stats() }
func (s *Sim) DBStats() PoolStats  { return s.db.Stats() }
