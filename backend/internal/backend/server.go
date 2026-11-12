// Package backend is one simulated application server. It exposes work
// endpoints of different "job sizes" plus /stats and a resource-control API
// that the load balancer uses.
package backend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"loadbalancer/internal/sim"
)

const (
	MaxCores  = 256
	MaxDBPool = 1000
	MaxFaultS = 600
)

// Stats is what GET /stats returns. The load balancer decodes the same type.
type Stats struct {
	Name     string        `json:"name"`
	Cores    int           `json:"cores"`
	DBPool   int           `json:"db_pool"`
	CPU      sim.PoolStats `json:"cpu"`
	DB       sim.PoolStats `json:"db"`
	Inflight int64         `json:"inflight"`
	Served   uint64        `json:"served"`
	UptimeS  float64       `json:"uptime_s"`
	DownForS float64       `json:"down_for_s"` // >0 = simulated outage, seconds left
}

// Fault is the body of PUT /admin/fault. It simulates an outage: for
// DownForS seconds every work endpoint, /health and /stats answer 503, the
// way a crashed app behind a still-open port does. 0 recovers immediately.
type Fault struct {
	DownForS int `json:"down_for_s"`
}

// Resources is the body of PUT /admin/resources. Nil fields are left unchanged.
type Resources struct {
	Cores  *int `json:"cores,omitempty"`
	DBPool *int `json:"db_pool,omitempty"`
}

type Server struct {
	name      string
	sim       *sim.Sim
	inflight  atomic.Int64
	served    atomic.Uint64
	started   time.Time
	downUntil atomic.Int64 // unix ns; 0 = up
}

// downFor returns how long the simulated outage still lasts (0 = up).
func (s *Server) downFor() time.Duration {
	u := s.downUntil.Load()
	if u == 0 {
		return 0
	}
	return max(time.Until(time.Unix(0, u)), 0)
}

func New(name string, cores, dbPool int) *Server {
	return &Server{name: name, sim: sim.New(cores, dbPool), started: time.Now()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home) // exact "/" only, not a catch-all
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("PUT /admin/resources", s.setResources)
	mux.HandleFunc("PUT /admin/fault", s.setFault)

	// Work endpoints, smallest job to largest:
	//   /browse   ~20 ms  (mostly DB)
	//   /checkout ~55 ms  (3 DB queries)
	//   /search   ~100 ms (pure CPU)
	mux.HandleFunc("GET /browse", s.work(s.browse))
	mux.HandleFunc("GET /checkout", s.work(s.checkout))
	mux.HandleFunc("GET /search", s.work(s.search))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// work counts in-flight and served requests around a work handler.
func (s *Server) work(h func() time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.downFor() > 0 {
			http.Error(w, s.name+" is down (simulated outage)", http.StatusServiceUnavailable)
			return
		}
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
		start := time.Now()
		waited := h()
		s.served.Add(1)
		total := time.Since(start)
		// Standard Server-Timing header: "work" is pure processing time,
		// excluding waits for a core / DB connection. The LB uses it as the
		// job length for SJF, so it isn't fooled by queueing under load.
		w.Header().Set("Server-Timing", fmt.Sprintf("work;dur=%.2f, wait;dur=%.2f",
			float64(total-waited)/1e6, float64(waited)/1e6))
		writeJSON(w, http.StatusOK, map[string]any{
			"name":      s.name,
			"waited_ms": waited.Milliseconds(),
			"total_ms":  time.Since(start).Milliseconds(),
		})
	}
}

// search is a pure-CPU job: one 100 ms CPU step.
func (s *Server) search() time.Duration {
	return s.sim.CPU(100 * time.Millisecond)
}

// browse: a little CPU, one DB query, a little CPU.
func (s *Server) browse() time.Duration {
	waited := s.sim.CPU(2 * time.Millisecond)
	waited += s.sim.DB(15 * time.Millisecond)
	waited += s.sim.CPU(3 * time.Millisecond)
	return waited
}

// checkout: validate, three DB writes, render.
func (s *Server) checkout() time.Duration {
	waited := s.sim.CPU(5 * time.Millisecond)
	for range 3 {
		waited += s.sim.DB(15 * time.Millisecond)
	}
	waited += s.sim.CPU(5 * time.Millisecond)
	return waited
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Welcome to %s\n", s.name)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if s.downFor() > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "name": s.name})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "name": s.name})
}

func (s *Server) Stats() Stats {
	cpu, db := s.sim.CPUStats(), s.sim.DBStats()
	return Stats{
		Name:     s.name,
		Cores:    cpu.Size,
		DBPool:   db.Size,
		CPU:      cpu,
		DB:       db,
		Inflight: s.inflight.Load(),
		Served:   s.served.Load(),
		UptimeS:  time.Since(s.started).Seconds(),
		DownForS: s.downFor().Seconds(),
	}
}

// stats answers 503 during a simulated outage, so health checks fail.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st := s.Stats()
	status := http.StatusOK
	if st.DownForS > 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, st)
}

func (s *Server) setFault(w http.ResponseWriter, r *http.Request) {
	var req Fault
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.DownForS < 0 || req.DownForS > MaxFaultS {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("down_for_s must be 0..%d", MaxFaultS)})
		return
	}
	if req.DownForS == 0 {
		s.downUntil.Store(0)
	} else {
		s.downUntil.Store(time.Now().Add(time.Duration(req.DownForS) * time.Second).UnixNano())
	}
	slog.Info("fault", "backend", s.name, "down_for_s", req.DownForS)
	writeJSON(w, http.StatusOK, s.Stats())
}

func (s *Server) setResources(w http.ResponseWriter, r *http.Request) {
	var req Resources
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Cores != nil && (*req.Cores < 1 || *req.Cores > MaxCores) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("cores must be 1..%d", MaxCores)})
		return
	}
	if req.DBPool != nil && (*req.DBPool < 1 || *req.DBPool > MaxDBPool) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("db_pool must be 1..%d", MaxDBPool)})
		return
	}
	if req.Cores != nil {
		s.sim.SetCores(*req.Cores)
	}
	if req.DBPool != nil {
		s.sim.SetDBPool(*req.DBPool)
	}
	writeJSON(w, http.StatusOK, s.Stats())
}
