package lb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- spec ------------------------------------------------------------------

// Variant is one configuration in a comparison: an algorithm, optionally
// with its own SJF aging (so "SJF without aging" can be compared to "SJF").
type Variant struct {
	Name      string    `json:"name"`
	Algorithm Algorithm `json:"algorithm"`
	Aging     *float64  `json:"aging,omitempty"`
}

// ExperimentSpec describes a controlled experiment.
//
//	kind "compare": every variant runs at the same load, with the same seeded
//	request sequence, for the same duration, Repeats times (order rotated each
//	repeat). Results are reported as median and min..max across repeats.
//
//	kind "theory": open-loop Poisson arrivals at several utilizations against
//	a single central FIFO queue (FCFS, one slot per core), compared with the
//	M/M/c (Erlang C) and M/G/c (Allen-Cunneen) predictions.
type ExperimentSpec struct {
	Kind      string         `json:"kind"`
	Variants  []Variant      `json:"variants,omitempty"`
	Users     int            `json:"users,omitempty"`
	ThinkMs   int            `json:"think_ms,omitempty"`
	Mix       map[string]int `json:"mix,omitempty"`
	Seed      uint64         `json:"seed"`
	DurationS int            `json:"duration_s"`
	WarmupS   int            `json:"warmup_s"`
	Repeats   int            `json:"repeats"`

	Path         string    `json:"path,omitempty"`         // theory: the endpoint to load (default /search)
	Utilizations []float64 `json:"utilizations,omitempty"` // theory: target utilizations, e.g. 0.5..0.95
}

func (s *ExperimentSpec) normalize(lim Limits) error {
	if s.DurationS == 0 {
		s.DurationS = 60
	}
	if s.WarmupS == 0 {
		s.WarmupS = 10
	}
	if s.Repeats == 0 {
		s.Repeats = 3
	}
	if s.Seed == 0 {
		s.Seed = 42
	}
	if s.DurationS < 5 || s.DurationS > 600 || s.WarmupS < 0 || s.WarmupS > 120 || s.Repeats < 1 || s.Repeats > 10 {
		return errors.New("duration_s must be 5..600, warmup_s 0..120, repeats 1..10")
	}
	switch s.Kind {
	case "compare":
		if len(s.Variants) == 0 {
			a := 0.02
			s.Variants = []Variant{{Name: "Round robin", Algorithm: RoundRobin}, {Name: "Least conn", Algorithm: LeastConn},
				{Name: "FCFS", Algorithm: FCFS}, {Name: "SJF", Algorithm: SJF, Aging: &a}}
		}
		if len(s.Variants) > 6 {
			return errors.New("at most 6 variants")
		}
		seen := map[string]bool{}
		for i, v := range s.Variants {
			if _, err := ParseAlgorithm(string(v.Algorithm)); err != nil {
				return err
			}
			if v.Aging != nil && (*v.Aging < 0 || *v.Aging > 10) {
				return errors.New("aging must be 0..10")
			}
			if v.Name == "" {
				s.Variants[i].Name = string(v.Algorithm)
			}
			if seen[s.Variants[i].Name] {
				return fmt.Errorf("duplicate variant name %q", s.Variants[i].Name)
			}
			seen[s.Variants[i].Name] = true
		}
		if s.Users == 0 {
			s.Users = 150
		}
		if s.Users < 1 || s.Users > limitOr(lim.MaxUsers, MaxUsers) {
			return fmt.Errorf("users must be 1..%d", limitOr(lim.MaxUsers, MaxUsers))
		}
		if len(s.Mix) == 0 {
			s.Mix = map[string]int{"/browse": 70, "/checkout": 10, "/search": 20}
		}
		if err := (LoadConfig{Users: s.Users, ThinkMs: s.ThinkMs, Mix: s.Mix}).Validate(); err != nil {
			return err
		}
	case "theory":
		if s.Path == "" {
			s.Path = "/search"
		}
		if !strings.HasPrefix(s.Path, "/") {
			return errors.New("path must start with /")
		}
		if len(s.Utilizations) == 0 {
			s.Utilizations = []float64{0.3, 0.5, 0.7, 0.8, 0.9, 0.95}
		}
		if len(s.Utilizations) > 10 {
			return errors.New("at most 10 utilizations")
		}
		for _, u := range s.Utilizations {
			if u <= 0 || u >= 1 || math.IsNaN(u) {
				return errors.New("utilizations must be between 0 and 1 (exclusive)")
			}
		}
		s.Repeats = 1
	default:
		return errors.New(`kind must be "compare" or "theory"`)
	}
	if maxS := lim.MaxExperimentS; maxS > 0 && s.totalSeconds() > maxS {
		return fmt.Errorf("this experiment would take %d s; this server allows %d s", s.totalSeconds(), maxS)
	}
	return nil
}

func (s *ExperimentSpec) steps() int {
	if s.Kind == "theory" {
		return len(s.Utilizations)
	}
	return len(s.Variants) * s.Repeats
}

// totalSeconds is the planned run time (plus a few seconds per step to drain).
func (s *ExperimentSpec) totalSeconds() int { return s.steps() * (s.DurationS + s.WarmupS + 3) }

// ---- results ---------------------------------------------------------------

type PathStats struct {
	Requests  int     `json:"requests"`
	MeanMs    float64 `json:"mean_ms"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
	MaxMs     float64 `json:"max_ms"`
	MaxWaitMs float64 `json:"max_wait_ms"` // longest time in the LB queue
}

type RunResult struct {
	Variant    string               `json:"variant"`
	Repeat     int                  `json:"repeat"`
	Seed       uint64               `json:"seed"`
	Throughput float64              `json:"throughput"` // successful requests per second
	Errors     int                  `json:"errors"`
	Shed       int                  `json:"shed"`
	Overall    PathStats            `json:"overall"`
	Paths      map[string]PathStats `json:"paths"`
}

// Spread = median and range across repeats.
type Spread struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

type PathSummary struct {
	P50     Spread `json:"p50_ms"`
	P95     Spread `json:"p95_ms"`
	P99     Spread `json:"p99_ms"`
	Max     Spread `json:"max_ms"`
	MaxWait Spread `json:"max_wait_ms"`
}

type VariantSummary struct {
	Variant    string                 `json:"variant"`
	Algorithm  Algorithm              `json:"algorithm"`
	Runs       int                    `json:"runs"`
	Throughput Spread                 `json:"throughput"`
	Errors     Spread                 `json:"errors"`
	Shed       Spread                 `json:"shed"`
	Overall    PathSummary            `json:"overall"`
	Paths      map[string]PathSummary `json:"paths"`
}

// TheoryPoint = one utilization step of the theory check.
type TheoryPoint struct {
	TargetRho   float64 `json:"target_rho"`
	Rho         float64 `json:"rho"` // measured: arrival rate × mean service ÷ servers
	Rate        float64 `json:"rate"`
	Servers     int     `json:"servers"`
	ServiceMs   float64 `json:"service_ms"` // measured mean work time
	ServiceCV2  float64 `json:"service_cv2"`
	Requests    int     `json:"requests"`
	WqMs        float64 `json:"wq_ms"`        // measured mean wait in the LB queue
	RespMs      float64 `json:"resp_ms"`      // measured mean response time
	PWait       float64 `json:"p_wait"`       // measured share of requests that had to queue
	ErlangC     float64 `json:"erlang_c"`     // predicted share that queues (M/M/c)
	MMcWqMs     float64 `json:"mmc_wq_ms"`    // M/M/c (Erlang C) predicted mean wait
	MGcWqMs     float64 `json:"mgc_wq_ms"`    // M/G/c (Allen-Cunneen) predicted mean wait
	ErrorPct    float64 `json:"error_pct"`    // (measured − M/G/c) ÷ M/G/c
	DroppedOpen uint64  `json:"dropped_open"` // arrivals the generator dropped (should be 0)
}

type Experiment struct {
	ID       string           `json:"id"`
	Spec     ExperimentSpec   `json:"spec"`
	Status   string           `json:"status"` // running | done | cancelled | failed
	Error    string           `json:"error,omitempty"`
	Started  time.Time        `json:"started"`
	Finished *time.Time       `json:"finished,omitempty"`
	Step     int              `json:"step"`
	Steps    int              `json:"steps"`
	Current  string           `json:"current"`
	ETAS     int              `json:"eta_s"`
	Runs     []RunResult      `json:"runs,omitempty"`
	Summary  []VariantSummary `json:"summary,omitempty"`
	Theory   []TheoryPoint    `json:"theory,omitempty"`
	Finding  string           `json:"finding,omitempty"`
}

// ---- runner ----------------------------------------------------------------

type experiments struct {
	mu      sync.Mutex
	list    []*Experiment // newest last
	running *Experiment
	cancel  context.CancelFunc
	dir     string // where finished results are saved as JSON ("" = memory only)
}

const keepExperiments = 20

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// ExperimentRunning reports whether controls are locked by an experiment.
func (l *LB) ExperimentRunning() bool {
	l.exps.mu.Lock()
	defer l.exps.mu.Unlock()
	return l.exps.running != nil
}

// StartExperiment validates spec and runs it in the background.
func (l *LB) StartExperiment(spec ExperimentSpec) (*Experiment, error) {
	if err := spec.normalize(l.Limits); err != nil {
		return nil, err
	}
	l.exps.mu.Lock()
	defer l.exps.mu.Unlock()
	if l.exps.running != nil {
		return nil, fmt.Errorf("experiment %s is already running", l.exps.running.ID)
	}
	e := &Experiment{ID: newID(), Spec: spec, Status: "running", Started: time.Now(), Steps: spec.steps(), ETAS: spec.totalSeconds()}
	ctx, cancel := context.WithCancel(l.runCtx)
	l.exps.running, l.exps.cancel = e, cancel
	l.exps.list = append(l.exps.list, e)
	if len(l.exps.list) > keepExperiments {
		l.exps.list = slices.Delete(l.exps.list, 0, len(l.exps.list)-keepExperiments)
	}
	slog.Info("experiment started", "id", e.ID, "kind", spec.Kind, "steps", e.Steps, "planned_s", e.ETAS)
	go l.runExperiment(ctx, e)
	return e.snapshot(), nil
}

func (l *LB) CancelExperiment(id string) bool {
	l.exps.mu.Lock()
	defer l.exps.mu.Unlock()
	if l.exps.running == nil || l.exps.running.ID != id {
		return false
	}
	l.exps.cancel()
	return true
}

func (l *LB) Experiments() []*Experiment {
	l.exps.mu.Lock()
	defer l.exps.mu.Unlock()
	out := make([]*Experiment, 0, len(l.exps.list))
	for i := len(l.exps.list) - 1; i >= 0; i-- {
		s := l.exps.list[i].snapshot()
		s.Runs, s.Summary, s.Theory = nil, nil, nil // list view: headline only
		out = append(out, s)
	}
	return out
}

func (l *LB) Experiment(id string) *Experiment {
	l.exps.mu.Lock()
	defer l.exps.mu.Unlock()
	for _, e := range l.exps.list {
		if e.ID == id {
			return e.snapshot()
		}
	}
	return nil
}

// snapshot copies an experiment for readers (caller holds exps.mu).
func (e *Experiment) snapshot() *Experiment {
	c := *e
	c.Runs = slices.Clone(e.Runs)
	c.Summary = slices.Clone(e.Summary)
	c.Theory = slices.Clone(e.Theory)
	return &c
}

func (l *LB) progress(e *Experiment, f func()) {
	l.exps.mu.Lock()
	f()
	l.exps.mu.Unlock()
}

// saved is the LB configuration an experiment changes, restored afterwards.
type saved struct {
	load        LoadConfig
	algo        Algorithm
	aging       float64
	maxQueue    int
	timeout     time.Duration
	maxInflight map[*Backend]int
}

func (l *LB) save() saved {
	st := l.Bal.Snapshot()
	sv := saved{load: l.Load.Config(), algo: st.Algorithm, aging: st.Aging, maxQueue: st.MaxQueue,
		timeout: time.Duration(st.QueueTimeoutMs) * time.Millisecond, maxInflight: map[*Backend]int{}}
	l.Bal.mu.Lock()
	for _, b := range l.Bal.backends {
		sv.maxInflight[b] = b.maxInflight
	}
	l.Bal.mu.Unlock()
	return sv
}

func (l *LB) restore(sv saved) {
	l.Load.Set(LoadConfig{Mix: sv.load.Mix, ThinkMs: sv.load.ThinkMs}) // stop first, then resume
	for b, n := range sv.maxInflight {
		l.Bal.SetMaxInflight(b, n)
	}
	l.Bal.SetQueueLimits(sv.maxQueue, sv.timeout)
	l.Bal.SetAging(sv.aging)
	l.Bal.SetAlgorithm(sv.algo)
	l.Load.Set(sv.load)
}

// drain stops generated traffic and waits for the queue and backends to empty.
func (l *LB) drain(ctx context.Context, mix map[string]int) {
	l.Load.Set(LoadConfig{Mix: mix})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		st := l.Bal.Snapshot()
		busy := st.QueueLen
		for _, b := range st.Backends {
			busy += b.Inflight
		}
		if busy == 0 {
			return
		}
		sleepCtx(ctx, 100*time.Millisecond)
	}
}

// recorder collects samples from the metrics tap during a measurement window.
type recorder struct {
	mu      sync.Mutex
	samples []Sample
}

func (r *recorder) add(s Sample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

// measure installs a tap for d and returns everything it saw.
func (l *LB) measure(ctx context.Context, d time.Duration) ([]Sample, error) {
	rec := &recorder{}
	l.Metrics.SetTap(rec.add)
	ok := sleepCtx(ctx, d)
	l.Metrics.SetTap(nil)
	if !ok {
		return nil, ctx.Err()
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.samples, nil
}

func (l *LB) runExperiment(ctx context.Context, e *Experiment) {
	sv := l.save()
	var err error
	if e.Spec.Kind == "theory" {
		err = l.runTheory(ctx, e)
	} else {
		err = l.runCompare(ctx, e)
	}
	l.drain(context.Background(), sv.load.Mix)
	l.restore(sv)

	now := time.Now()
	l.progress(e, func() {
		e.Finished, e.Current, e.ETAS = &now, "", 0
		switch {
		case errors.Is(err, context.Canceled):
			e.Status = "cancelled"
		case err != nil:
			e.Status, e.Error = "failed", err.Error()
		default:
			e.Status = "done"
		}
		l.exps.running, l.exps.cancel = nil, nil
	})
	slog.Info("experiment finished", "id", e.ID, "status", e.Status, "finding", e.Finding, "took", time.Since(e.Started).Round(time.Second).String())
	l.saveExperiment(e)
}

// loadExperiments reads finished experiments saved by an earlier run, so
// results survive restarts and redeploys (the directory is a volume).
func (x *experiments) load() {
	if x.dir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(x.dir, "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e Experiment
		if json.Unmarshal(data, &e) != nil || e.ID == "" || e.Status != "done" {
			slog.Warn("skipping unreadable experiment file", "file", f)
			continue
		}
		x.list = append(x.list, &e)
	}
	slices.SortFunc(x.list, func(a, b *Experiment) int { return a.Started.Compare(b.Started) })
	if len(x.list) > keepExperiments {
		x.list = slices.Delete(x.list, 0, len(x.list)-keepExperiments)
	}
	if len(x.list) > 0 {
		slog.Info("loaded saved experiments", "count", len(x.list), "dir", x.dir)
	}
}

func (l *LB) saveExperiment(e *Experiment) {
	if l.exps.dir == "" || e.Status != "done" {
		return
	}
	l.exps.mu.Lock()
	data, _ := json.MarshalIndent(e, "", "  ")
	l.exps.mu.Unlock()
	if err := os.MkdirAll(l.exps.dir, 0o755); err == nil {
		err = os.WriteFile(filepath.Join(l.exps.dir, e.ID+".json"), data, 0o644)
		if err != nil {
			slog.Warn("could not save experiment", "id", e.ID, "error", err.Error())
		}
	}
}

func (l *LB) runCompare(ctx context.Context, e *Experiment) error {
	sp := e.Spec
	step := 0
	for r := range sp.Repeats {
		seed := sp.Seed + uint64(r) // same within a repeat, different across repeats
		n := len(sp.Variants)
		for k := range n {
			v := sp.Variants[(k+r)%n] // rotate the order each repeat: no variant always runs first
			step++
			left := (sp.steps() - step + 1) * (sp.DurationS + sp.WarmupS + 3)
			l.progress(e, func() {
				e.Step, e.Current, e.ETAS = step, fmt.Sprintf("%s · repeat %d of %d", v.Name, r+1, sp.Repeats), left
			})

			l.drain(ctx, sp.Mix)
			if v.Aging != nil {
				l.Bal.SetAging(*v.Aging)
			}
			l.Bal.SetAlgorithm(v.Algorithm)
			if err := l.Load.Set(LoadConfig{Users: sp.Users, ThinkMs: sp.ThinkMs, Mix: sp.Mix, Seed: seed}); err != nil {
				return err
			}
			if !sleepCtx(ctx, time.Duration(sp.WarmupS)*time.Second) {
				return ctx.Err()
			}
			samples, err := l.measure(ctx, time.Duration(sp.DurationS)*time.Second)
			if err != nil {
				return err
			}
			run := summarizeRun(samples, sp.DurationS)
			run.Variant, run.Repeat, run.Seed = v.Name, r+1, seed
			l.progress(e, func() { e.Runs = append(e.Runs, run) })
		}
	}
	l.progress(e, func() {
		e.Summary = summarizeVariants(sp.Variants, e.Runs)
		e.Finding = compareFinding(e.Summary)
	})
	return nil
}

func statsOf(lat, wait []float64) PathStats {
	if len(lat) == 0 {
		return PathStats{}
	}
	slices.Sort(lat)
	var sum float64
	for _, x := range lat {
		sum += x
	}
	ps := PathStats{Requests: len(lat), MeanMs: sum / float64(len(lat)), P50Ms: quantile(lat, 0.5),
		P95Ms: quantile(lat, 0.95), P99Ms: quantile(lat, 0.99), MaxMs: lat[len(lat)-1]}
	if len(wait) > 0 {
		ps.MaxWaitMs = slices.Max(wait)
	}
	return ps
}

func summarizeRun(samples []Sample, durationS int) RunResult {
	var all, allWait []float64
	byPath := map[string][2][]float64{}
	run := RunResult{Paths: map[string]PathStats{}}
	for _, s := range samples {
		switch s.Outcome {
		case OutcomeOK:
			ms, w := float64(s.Latency)/1e6, float64(s.Wait)/1e6
			all, allWait = append(all, ms), append(allWait, w)
			p := byPath[s.Path]
			p[0], p[1] = append(p[0], ms), append(p[1], w)
			byPath[s.Path] = p
		case OutcomeBackendError, OutcomeNoBackend:
			run.Errors++
		case OutcomeQueueFull, OutcomeQueueTimeout:
			run.Shed++
		}
	}
	run.Throughput = float64(len(all)) / float64(durationS)
	run.Overall = statsOf(all, allWait)
	for p, v := range byPath {
		run.Paths[p] = statsOf(v[0], v[1])
	}
	return run
}

func spreadOf(xs []float64) Spread {
	if len(xs) == 0 {
		return Spread{}
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return Spread{Median: med, Min: s[0], Max: s[len(s)-1]}
}

func pathSummary(stats []PathStats) PathSummary {
	col := func(f func(PathStats) float64) Spread {
		xs := make([]float64, 0, len(stats))
		for _, s := range stats {
			xs = append(xs, f(s))
		}
		return spreadOf(xs)
	}
	return PathSummary{
		P50: col(func(s PathStats) float64 { return s.P50Ms }), P95: col(func(s PathStats) float64 { return s.P95Ms }),
		P99: col(func(s PathStats) float64 { return s.P99Ms }), Max: col(func(s PathStats) float64 { return s.MaxMs }),
		MaxWait: col(func(s PathStats) float64 { return s.MaxWaitMs }),
	}
}

func summarizeVariants(variants []Variant, runs []RunResult) []VariantSummary {
	var out []VariantSummary
	for _, v := range variants {
		var tp, errs, shed []float64
		var overall []PathStats
		paths := map[string][]PathStats{}
		for _, r := range runs {
			if r.Variant != v.Name {
				continue
			}
			tp, errs, shed = append(tp, r.Throughput), append(errs, float64(r.Errors)), append(shed, float64(r.Shed))
			overall = append(overall, r.Overall)
			for p, s := range r.Paths {
				paths[p] = append(paths[p], s)
			}
		}
		vs := VariantSummary{Variant: v.Name, Algorithm: v.Algorithm, Runs: len(tp), Throughput: spreadOf(tp),
			Errors: spreadOf(errs), Shed: spreadOf(shed), Overall: pathSummary(overall), Paths: map[string]PathSummary{}}
		for p, s := range paths {
			vs.Paths[p] = pathSummary(s)
		}
		out = append(out, vs)
	}
	return out
}

// compareFinding states the headline trade-off in one sentence: the biggest
// p95 improvement any variant achieved over the baseline (FCFS if present,
// else the first variant), and what that variant made worse.
func compareFinding(sum []VariantSummary) string {
	if len(sum) < 2 {
		return ""
	}
	base := sum[0]
	for _, v := range sum {
		if v.Algorithm == FCFS {
			base = v
			break
		}
	}
	pct := func(newV, oldV float64) float64 {
		if oldV == 0 {
			return 0
		}
		return (newV - oldV) / oldV * 100
	}
	type change struct {
		variant, path string
		p95, p99      float64
	}
	var best *change
	var worst *change
	for _, v := range sum {
		if v.Variant == base.Variant {
			continue
		}
		paths := make([]string, 0, len(v.Paths))
		for p := range v.Paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			b, ok := base.Paths[p]
			if !ok {
				continue
			}
			c := change{v.Variant, p, pct(v.Paths[p].P95.Median, b.P95.Median), pct(v.Paths[p].P99.Median, b.P99.Median)}
			if best == nil || c.p95 < best.p95 {
				cc := c
				best = &cc
			}
		}
	}
	if best == nil {
		return ""
	}
	for _, v := range sum {
		if v.Variant != best.variant {
			continue
		}
		for p, s := range v.Paths {
			c := change{v.Variant, p, 0, pct(s.P99.Median, base.Paths[p].P99.Median)}
			if worst == nil || c.p99 > worst.p99 {
				cc := c
				worst = &cc
			}
		}
	}
	msg := fmt.Sprintf("%s cut %s p95 by %.0f%% vs %s", best.variant, best.path, -best.p95, base.Variant)
	if best.p95 >= 0 {
		msg = fmt.Sprintf("No variant beat %s on p95 for any endpoint", base.Variant)
	}
	if worst != nil && worst.p99 > 5 && best.p95 < 0 {
		msg += fmt.Sprintf(", but raised %s p99 by %.0f%%", worst.path, worst.p99)
	}
	return msg + "."
}

// ---- theory check ----------------------------------------------------------

// erlangC = probability an arrival has to wait in an M/M/c queue with c
// servers and offered load a = λ·S (in Erlangs).
func erlangC(c int, a float64) float64 {
	if c <= 0 {
		return 1
	}
	rho := a / float64(c)
	if rho >= 1 {
		return 1
	}
	term, sum := 1.0, 1.0 // a^k/k!, accumulated for k < c
	for k := 1; k < c; k++ {
		term *= a / float64(k)
		sum += term
	}
	top := term * a / float64(c) / (1 - rho)
	return top / (sum + top)
}

// mmcWait = mean time in queue for M/M/c, in the units of service.
func mmcWait(c int, a, service float64) float64 {
	if a >= float64(c) {
		return math.Inf(1)
	}
	return erlangC(c, a) * service / (float64(c) - a)
}

func (l *LB) runTheory(ctx context.Context, e *Experiment) error {
	sp := e.Spec
	// A single central FIFO queue in front of all cores: FCFS, one LB slot
	// per core. That is exactly the M/x/c model: c servers, one queue.
	servers := 0
	l.Bal.mu.Lock()
	for _, b := range l.Bal.backends {
		if b.usable() {
			cores := 1
			if st := b.stats.Load(); st != nil && st.Cores > 0 {
				cores = st.Cores
			}
			b.maxInflight = cores
			servers += cores
		}
	}
	l.Bal.mu.Unlock()
	if servers == 0 {
		return errors.New("no healthy backends")
	}
	l.Bal.SetQueueLimits(1_000_000, 120*time.Second)
	l.Bal.SetAlgorithm(FCFS)
	mix := map[string]int{sp.Path: 1}

	service := l.Est.Estimate(sp.Path) / 1000 // seconds; refined from measurements below
	if service <= 0 {
		service = 0.1
	}
	for i, target := range sp.Utilizations {
		left := (len(sp.Utilizations) - i) * (sp.DurationS + sp.WarmupS + 3)
		l.progress(e, func() {
			e.Step, e.Current, e.ETAS = i+1, fmt.Sprintf("utilization %.0f%%", target*100), left
		})
		l.drain(ctx, mix)
		rate := target * float64(servers) / service
		dropped := l.Load.dropped.Load()
		if err := l.Load.Set(LoadConfig{Mix: mix, Seed: sp.Seed + uint64(i), Mode: "open", Rate: rate}); err != nil {
			return err
		}
		if !sleepCtx(ctx, time.Duration(sp.WarmupS)*time.Second) {
			return ctx.Err()
		}
		samples, err := l.measure(ctx, time.Duration(sp.DurationS)*time.Second)
		if err != nil {
			return err
		}
		pt := theoryPoint(samples, sp.DurationS, servers)
		pt.TargetRho, pt.Rate, pt.DroppedOpen = target, rate, l.Load.dropped.Load()-dropped
		if pt.ServiceMs > 0 {
			service = pt.ServiceMs / 1000 // later steps aim more accurately
		}
		l.progress(e, func() { e.Theory = append(e.Theory, pt) })
	}
	l.progress(e, func() { e.Finding = theoryFinding(e.Theory) })
	return nil
}

func theoryPoint(samples []Sample, durationS, servers int) TheoryPoint {
	var n, waited int
	var wq, resp, work, work2 float64
	for _, s := range samples {
		if s.Outcome != OutcomeOK {
			continue
		}
		n++
		w := float64(s.Wait) / 1e6
		wq += w
		resp += float64(s.Latency) / 1e6
		x := float64(s.Work) / 1e6
		work += x
		work2 += x * x
		if w > 1 { // waited more than 1 ms for a free core
			waited++
		}
	}
	pt := TheoryPoint{Servers: servers, Requests: n}
	if n == 0 {
		return pt
	}
	fn := float64(n)
	pt.WqMs, pt.RespMs, pt.PWait = wq/fn, resp/fn, float64(waited)/fn
	pt.ServiceMs = work / fn
	if pt.ServiceMs > 0 {
		variance := work2/fn - pt.ServiceMs*pt.ServiceMs
		pt.ServiceCV2 = max(variance, 0) / (pt.ServiceMs * pt.ServiceMs)
	}
	lambda := fn / float64(durationS) // measured arrivals per second
	a := lambda * pt.ServiceMs / 1000 // offered load, Erlangs
	pt.Rho = a / float64(servers)
	pt.ErlangC = erlangC(servers, a)
	pt.MMcWqMs = mmcWait(servers, a, pt.ServiceMs)
	// Allen-Cunneen: Wq(M/G/c) ≈ Wq(M/M/c) × (Ca² + Cs²)/2, with Poisson
	// arrivals (Ca² = 1) and the measured service variability Cs².
	pt.MGcWqMs = pt.MMcWqMs * (1 + pt.ServiceCV2) / 2
	if pt.MGcWqMs > 0 && !math.IsInf(pt.MGcWqMs, 1) {
		pt.ErrorPct = (pt.WqMs - pt.MGcWqMs) / pt.MGcWqMs * 100
	}
	return pt
}

// theoryFinding leads with where measurement and M/G/c agree, then the
// biggest disagreement. Steps whose predicted wait is under 5 ms are left
// out: there, timer and network noise is as large as the effect.
func theoryFinding(pts []TheoryPoint) string {
	var close []TheoryPoint
	var far *TheoryPoint
	worstClose := 0.0
	for i := range pts {
		p := pts[i]
		if p.MGcWqMs < 5 {
			continue
		}
		if math.Abs(p.ErrorPct) <= 10 {
			close = append(close, p)
			worstClose = max(worstClose, math.Abs(p.ErrorPct))
		} else if far == nil || math.Abs(p.ErrorPct) > math.Abs(far.ErrorPct) {
			far = &pts[i]
		}
	}
	var parts []string
	if len(close) > 0 {
		ratio := close[0].MMcWqMs / close[0].MGcWqMs
		parts = append(parts, fmt.Sprintf("From %.0f%% to %.0f%% utilization the measured queue wait was within %.0f%% of M/G/c (plain M/M/c predicts %.1fx more, because job times here are nearly constant)",
			close[0].Rho*100, close[len(close)-1].Rho*100, max(worstClose, 1), ratio))
	}
	if far != nil {
		parts = append(parts, fmt.Sprintf("at %.0f%% it was %.0f ms vs %.0f ms predicted (%+.0f%%)", far.Rho*100, far.WqMs, far.MGcWqMs, far.ErrorPct))
	}
	if len(parts) == 0 {
		return "Queue waits were too small to compare with the model at these utilizations."
	}
	return strings.Join(parts, "; ") + "."
}
