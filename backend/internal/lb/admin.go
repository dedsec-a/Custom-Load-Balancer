package lb

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"loadbalancer/internal/backend"
)

// AdminHandler is the control-plane API the front-end talks to.
// Serve it on a separate, non-public port.
//
//	GET    /api/state                       everything below in one call
//	PUT    /api/algorithm                   {"algorithm":"sjf","aging":0.1}
//	GET    /api/backends
//	POST   /api/backends                    {"name":"b4","url":"http://127.0.0.1:3004"}
//	DELETE /api/backends/{name}
//	PUT    /api/backends/{name}/resources   {"cores":8,"db_pool":20,"max_inflight":0}
//	PUT    /api/resources                   same body, applied to every backend
//	GET    /api/load
//	PUT    /api/load                        {"users":200,"think_ms":0,"mix":{"/browse":70,"/search":30}}
//	GET    /api/metrics?last=120            last N seconds of Points
//	GET    /api/stream                      Server-Sent Events, one Point per second
//
// If uiDir is set, the built dashboard is served from "/" on the same port.
// If password is set, everything requires HTTP Basic auth (user "admin").
func (l *LB) AdminHandler(corsOrigin, uiDir, password string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /metrics", l.ServeMetrics) // Prometheus; blocked from the internet in the Caddyfile
	mux.HandleFunc("GET /api/state", l.getState)
	mux.HandleFunc("PUT /api/algorithm", l.putAlgorithm)
	mux.HandleFunc("GET /api/backends", l.getBackends)
	mux.HandleFunc("POST /api/backends", l.postBackend)
	mux.HandleFunc("DELETE /api/backends/{name}", l.deleteBackend)
	mux.HandleFunc("PUT /api/backends/{name}/resources", l.putBackendResources)
	mux.HandleFunc("PUT /api/backends/{name}/fault", l.putFault)
	mux.HandleFunc("PUT /api/resources", l.putAllResources)
	mux.HandleFunc("GET /api/load", l.getLoad)
	mux.HandleFunc("PUT /api/load", l.putLoad)
	mux.HandleFunc("GET /api/metrics", l.getMetrics)
	mux.HandleFunc("GET /api/stream", l.stream)
	mux.HandleFunc("GET /api/experiments", l.listExperiments)
	mux.HandleFunc("POST /api/experiments", l.postExperiment)
	mux.HandleFunc("GET /api/experiments/{id}", l.getExperiment)
	mux.HandleFunc("POST /api/experiments/{id}/cancel", l.cancelExperiment)
	if uiDir != "" {
		mux.Handle("GET /", spa(uiDir))
	}
	var h http.Handler = l.limitWrites(mux)
	h = cors(corsOrigin, h)
	if password != "" {
		h = basicAuth(password, h)
	}
	return h
}

// limitWrites rate-limits changes per client IP and logs every accepted one.
func (l *LB) limitWrites(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r, l.trustXFF)
		if l.ExperimentRunning() && !strings.HasPrefix(r.URL.Path, "/api/experiments") {
			writeErr(w, http.StatusConflict, errors.New("an experiment is running: controls are locked until it finishes or is cancelled"))
			return
		}
		if !l.adminLimiter.allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, errors.New("too many changes, slow down"))
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		slog.Info("admin change", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ip", ip)
	})
}

// spa serves static files, falling back to index.html for unknown paths.
func spa(dir string) http.Handler {
	fsys := os.DirFS(dir)
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(fsys, name); err != nil {
			http.ServeFileFS(w, r, fsys, "index.html")
			return
		}
		if strings.HasPrefix(name, "assets/") { // content-hashed by Vite
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

func basicAuth(password string, h http.Handler) http.Handler {
	want := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" { // probes and the private Prometheus scrape
			h.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		got := sha256.Sum256([]byte(pass))
		if !ok || user != "admin" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="load balancer", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func cors(origin string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("empty body")
		}
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return false
	}
	return true
}

type State struct {
	BalancerState
	Load       LoadConfig              `json:"load"`
	Estimates  map[string]PathEstimate `json:"estimates"`
	Limits     Limits                  `json:"limits"`
	Experiment *Experiment             `json:"experiment,omitempty"` // the running one, headline only
}

func (l *LB) State() State {
	s := State{BalancerState: l.Bal.Snapshot(), Load: l.Load.Config(), Estimates: l.Est.Snapshot(), Limits: l.Limits}
	l.exps.mu.Lock()
	if e := l.exps.running; e != nil {
		s.Experiment = e.snapshot()
		s.Experiment.Runs, s.Experiment.Summary, s.Experiment.Theory = nil, nil, nil
	}
	l.exps.mu.Unlock()
	return s
}

func (l *LB) listExperiments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, l.Experiments())
}

func (l *LB) getExperiment(w http.ResponseWriter, r *http.Request) {
	e := l.Experiment(r.PathValue("id"))
	if e == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no experiment %q", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (l *LB) postExperiment(w http.ResponseWriter, r *http.Request) {
	var spec ExperimentSpec
	if !readJSON(w, r, &spec) {
		return
	}
	e, err := l.StartExperiment(spec)
	if err != nil {
		status := http.StatusBadRequest
		if l.ExperimentRunning() {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	l.touched()
	writeJSON(w, http.StatusAccepted, e)
}

func (l *LB) cancelExperiment(w http.ResponseWriter, r *http.Request) {
	if !l.CancelExperiment(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, errors.New("that experiment is not running"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (l *LB) getState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, l.State())
}

func (l *LB) putAlgorithm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Algorithm      *string  `json:"algorithm"`
		Aging          *float64 `json:"aging"`
		MaxQueue       *int     `json:"max_queue"`
		QueueTimeoutMs *int     `json:"queue_timeout_ms"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var algo Algorithm
	if req.Algorithm != nil {
		a, err := ParseAlgorithm(*req.Algorithm)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		algo = a
	}
	if req.Aging != nil && (*req.Aging < 0 || *req.Aging > 10) {
		writeErr(w, http.StatusBadRequest, errors.New("aging must be 0..10"))
		return
	}
	if req.MaxQueue != nil && (*req.MaxQueue < 1 || *req.MaxQueue > 1_000_000) {
		writeErr(w, http.StatusBadRequest, errors.New("max_queue must be 1..1000000"))
		return
	}
	if req.QueueTimeoutMs != nil && (*req.QueueTimeoutMs < 1 || *req.QueueTimeoutMs > 300_000) {
		writeErr(w, http.StatusBadRequest, errors.New("queue_timeout_ms must be 1..300000"))
		return
	}
	if req.Aging != nil {
		l.Bal.SetAging(*req.Aging)
	}
	if req.MaxQueue != nil || req.QueueTimeoutMs != nil {
		mq, qt := 0, time.Duration(0)
		if req.MaxQueue != nil {
			mq = *req.MaxQueue
		}
		if req.QueueTimeoutMs != nil {
			qt = time.Duration(*req.QueueTimeoutMs) * time.Millisecond
		}
		l.Bal.SetQueueLimits(mq, qt)
	}
	if algo != "" {
		l.Bal.SetAlgorithm(algo)
	}
	l.touched()
	writeJSON(w, http.StatusOK, l.State())
}

func (l *LB) getBackends(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, l.Bal.Snapshot().Backends)
}

var errLocked = errors.New("adding and removing backends is disabled on this server")

func (l *LB) postBackend(w http.ResponseWriter, r *http.Request) {
	if l.Limits.LockBackends {
		writeErr(w, http.StatusForbidden, errLocked)
		return
	}
	var spec BackendSpec
	if !readJSON(w, r, &spec) {
		return
	}
	if err := l.AddBackend(spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b := l.Bal.Get(spec.Name)
	l.poll(r.Context(), b) // healthy right away if it is up
	l.Bal.Wake()
	writeJSON(w, http.StatusCreated, l.Bal.Snapshot().Backends)
}

func (l *LB) deleteBackend(w http.ResponseWriter, r *http.Request) {
	if l.Limits.LockBackends {
		writeErr(w, http.StatusForbidden, errLocked)
		return
	}
	if !l.Bal.Drain(r.PathValue("name")) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no backend %q", r.PathValue("name")))
		return
	}
	// 202: no new requests go to it; it leaves once its in-flight ones finish.
	writeJSON(w, http.StatusAccepted, l.Bal.Snapshot().Backends)
}

// putFault simulates an outage on one backend so failover can be watched.
// Allowed even in demo mode (it only targets the configured backends), but
// capped by Limits.MaxFaultS so a visitor can't leave one down for long.
func (l *LB) putFault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DownForS *int `json:"down_for_s"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	maxS := limitOr(l.Limits.MaxFaultS, backend.MaxFaultS)
	if req.DownForS == nil || *req.DownForS < 0 || *req.DownForS > maxS {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("down_for_s must be 0..%d (0 = recover now)", maxS))
		return
	}
	b := l.Bal.Get(r.PathValue("name"))
	if b == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no backend %q", r.PathValue("name")))
		return
	}
	if err := l.SetFault(r.Context(), b, *req.DownForS); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if *req.DownForS == 0 {
		l.poll(r.Context(), b) // bring it back now, not at the next tick
		l.Bal.Wake()
	}
	l.touched()
	writeJSON(w, http.StatusOK, l.Bal.Snapshot().Backends)
}

type resourcesReq struct {
	Cores       *int `json:"cores"`
	DBPool      *int `json:"db_pool"`
	MaxInflight *int `json:"max_inflight"` // 0 = auto
}

func limitOr(limit, fallback int) int {
	if limit > 0 {
		return min(limit, fallback)
	}
	return fallback
}

func (q resourcesReq) validate(lim Limits) error {
	if q.Cores == nil && q.DBPool == nil && q.MaxInflight == nil {
		return errors.New("set at least one of cores, db_pool, max_inflight")
	}
	if maxC := limitOr(lim.MaxCores, backend.MaxCores); q.Cores != nil && (*q.Cores < 1 || *q.Cores > maxC) {
		return fmt.Errorf("cores must be 1..%d", maxC)
	}
	if maxD := limitOr(lim.MaxDBPool, backend.MaxDBPool); q.DBPool != nil && (*q.DBPool < 1 || *q.DBPool > maxD) {
		return fmt.Errorf("db_pool must be 1..%d", maxD)
	}
	if q.MaxInflight != nil && (*q.MaxInflight < 0 || *q.MaxInflight > 100000) {
		return errors.New("max_inflight must be 0..100000 (0 = auto)")
	}
	return nil
}

func (l *LB) applyResources(ctx context.Context, b *Backend, q resourcesReq) error {
	if q.MaxInflight != nil {
		l.Bal.SetMaxInflight(b, *q.MaxInflight)
	}
	if q.Cores != nil || q.DBPool != nil {
		_, err := l.SetResources(ctx, b, backend.Resources{Cores: q.Cores, DBPool: q.DBPool})
		return err
	}
	return nil
}

func (l *LB) putBackendResources(w http.ResponseWriter, r *http.Request) {
	var q resourcesReq
	if !readJSON(w, r, &q) {
		return
	}
	if err := q.validate(l.Limits); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b := l.Bal.Get(r.PathValue("name"))
	if b == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no backend %q", r.PathValue("name")))
		return
	}
	if err := l.applyResources(r.Context(), b, q); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	l.touched()
	writeJSON(w, http.StatusOK, l.Bal.Snapshot().Backends)
}

func (l *LB) putAllResources(w http.ResponseWriter, r *http.Request) {
	var q resourcesReq
	if !readJSON(w, r, &q) {
		return
	}
	if err := q.validate(l.Limits); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, b := range l.Bal.Backends() {
		wg.Go(func() {
			if err := l.applyResources(r.Context(), b, q); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		writeErr(w, http.StatusBadGateway, errors.Join(errs...))
		return
	}
	l.touched()
	writeJSON(w, http.StatusOK, l.Bal.Snapshot().Backends)
}

func (l *LB) getLoad(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, l.Load.Config())
}

// putLoad merges: omitted fields keep their current value.
func (l *LB) putLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Users   *int           `json:"users"`
		ThinkMs *int           `json:"think_ms"`
		Mix     map[string]int `json:"mix"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	cfg := l.Load.Config()
	if req.Users != nil {
		cfg.Users = *req.Users
	}
	if req.ThinkMs != nil {
		cfg.ThinkMs = *req.ThinkMs
	}
	if req.Mix != nil {
		cfg.Mix = req.Mix
	}
	if maxU := limitOr(l.Limits.MaxUsers, MaxUsers); cfg.Users > maxU {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("users must be 0..%d on this server", maxU))
		return
	}
	if err := l.Load.Set(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	l.touched()
	writeJSON(w, http.StatusOK, l.Load.Config())
}

func (l *LB) getMetrics(w http.ResponseWriter, r *http.Request) {
	n := 0
	if s := r.URL.Query().Get("last"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("last must be a non-negative integer"))
			return
		}
		n = v
	}
	writeJSON(w, http.StatusOK, l.Metrics.History(n))
}

func (l *LB) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch, unsubscribe := l.Metrics.Subscribe()
	defer unsubscribe()
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case p := <-ch:
			data, _ := json.Marshal(p)
			if _, err := fmt.Fprintf(w, "event: point\ndata: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
