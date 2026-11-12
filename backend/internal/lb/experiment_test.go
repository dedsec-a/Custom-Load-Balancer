package lb

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http/httptest"
	"testing"
	"time"
)

func TestErlangCMatchesTextbook(t *testing.T) {
	// M/M/1: P(wait) = rho, Wq = rho/(1-rho) * S.
	if got := erlangC(1, 0.5); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("M/M/1 rho=0.5: C = %v, want 0.5", got)
	}
	if got := mmcWait(1, 0.8, 100); math.Abs(got-400) > 1e-6 {
		t.Fatalf("M/M/1 rho=0.8, S=100: Wq = %v, want 400", got)
	}
	// M/M/2 with a = 1 Erlang: C = 1/3.
	if got := erlangC(2, 1); math.Abs(got-1.0/3) > 1e-9 {
		t.Fatalf("M/M/2 a=1: C = %v, want 1/3", got)
	}
	if !math.IsInf(mmcWait(2, 2, 100), 1) {
		t.Fatal("saturated queue must predict an infinite wait")
	}
}

func TestSeededLoadIsReproducible(t *testing.T) {
	g := NewLoadGen("http://unused")
	g.Set(LoadConfig{Mix: map[string]int{"/a": 1, "/b": 1, "/c": 1}})
	seq := func(seed uint64) []string {
		rng := newRNG(seed, 7)
		var out []string
		for range 20 {
			p, _ := g.pick(rng)
			out = append(out, p)
		}
		return out
	}
	a, b, c := seq(42), seq(42), seq(43)
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("same seed produced a different request sequence")
		}
	}
	same := true
	for i := range a {
		same = same && a[i] == c[i]
	}
	if same {
		t.Fatal("different seeds produced the same sequence")
	}
	_ = rand.Uint64
}

func experimentCluster(t *testing.T, cores, db int) (*LB, *httptest.Server) {
	t.Helper()
	l, front := clusterWith(t, 2, cores, db, Config{Algorithm: Auto})
	l.Load = NewLoadGen(front.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l.runCtx = ctx
	l.Load.Start(ctx)
	return l, front
}

func waitExperiment(t *testing.T, l *LB, id string, within time.Duration) *Experiment {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if e := l.Experiment(id); e != nil && e.Status != "running" {
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("experiment %s did not finish in %v", id, within)
	return nil
}

func TestCompareExperimentEndToEnd(t *testing.T) {
	l, _ := experimentCluster(t, 2, 4)
	l.Load.Set(LoadConfig{Users: 3, Mix: map[string]int{"/browse": 1}}) // a pre-existing setting to restore
	admin := httptest.NewServer(l.AdminHandler("", "", ""))
	defer admin.Close()

	zero := 0.0
	e, err := l.StartExperiment(ExperimentSpec{Kind: "compare", Users: 20, DurationS: 5, WarmupS: 1, Repeats: 2,
		Variants: []Variant{{Name: "FCFS", Algorithm: FCFS}, {Name: "SJF", Algorithm: SJF, Aging: &zero}}})
	if err != nil {
		t.Fatal(err)
	}
	// Controls are locked while it runs; a second experiment is refused.
	if code := doJSON(t, "PUT", admin.URL+"/api/load", map[string]int{"users": 5}, nil); code != 409 {
		t.Fatalf("load change during experiment: got %d, want 409", code)
	}
	if _, err := l.StartExperiment(ExperimentSpec{Kind: "theory", DurationS: 5}); err == nil {
		t.Fatal("a second experiment started while one was running")
	}

	done := waitExperiment(t, l, e.ID, 60*time.Second)
	if done.Status != "done" {
		t.Fatalf("status %s: %s", done.Status, done.Error)
	}
	if len(done.Runs) != 4 || len(done.Summary) != 2 {
		t.Fatalf("want 4 runs and 2 summaries, got %d and %d", len(done.Runs), len(done.Summary))
	}
	// The run order rotates: repeat 2 starts with the other variant.
	if done.Runs[0].Variant == done.Runs[2].Variant {
		t.Fatalf("run order did not rotate: %s then %s", done.Runs[0].Variant, done.Runs[2].Variant)
	}
	// Paired comparison: both variants in a repeat used the same seed.
	if done.Runs[0].Seed != done.Runs[1].Seed || done.Runs[0].Seed == done.Runs[2].Seed {
		t.Fatal("seeds must match within a repeat and differ across repeats")
	}
	for _, s := range done.Summary {
		if s.Throughput.Median <= 0 || s.Overall.P95.Median <= 0 || s.Paths["/browse"].P95.Median <= 0 {
			t.Fatalf("empty summary for %s: %+v", s.Variant, s)
		}
		if s.Throughput.Min > s.Throughput.Median || s.Throughput.Median > s.Throughput.Max {
			t.Fatalf("spread out of order: %+v", s.Throughput)
		}
	}
	if done.Finding == "" {
		t.Fatal("no finding written")
	}
	// Previous settings are back.
	if c := l.Load.Config(); c.Users != 3 || c.Seed != 0 {
		t.Fatalf("load not restored: %+v", c)
	}
	if st := l.Bal.Snapshot(); st.Algorithm != Auto {
		t.Fatalf("algorithm not restored: %s", st.Algorithm)
	}
	if code := doJSON(t, "PUT", admin.URL+"/api/load", map[string]int{"users": 0}, nil); code != 200 {
		t.Fatalf("controls still locked after the experiment: %d", code)
	}
}

func TestTheoryExperiment(t *testing.T) {
	l, _ := experimentCluster(t, 2, 4) // 2 backends x 2 cores = 4 servers
	l.Est.Seed("/search", 100*time.Millisecond)
	e, err := l.StartExperiment(ExperimentSpec{Kind: "theory", DurationS: 6, WarmupS: 1, Utilizations: []float64{0.6}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitExperiment(t, l, e.ID, 40*time.Second)
	if done.Status != "done" || len(done.Theory) != 1 {
		t.Fatalf("status %s, %d points: %s", done.Status, len(done.Theory), done.Error)
	}
	p := done.Theory[0]
	if p.Servers != 4 || p.Requests < 50 {
		t.Fatalf("unexpected point: %+v", p)
	}
	if p.ServiceMs < 95 || p.ServiceMs > 130 {
		t.Fatalf("measured /search service time %.1f ms, want about 100", p.ServiceMs)
	}
	if math.Abs(p.Rho-0.6) > 0.2 {
		t.Fatalf("measured utilization %.2f, want about 0.6", p.Rho)
	}
	if p.MMcWqMs <= 0 || p.MGcWqMs >= p.MMcWqMs {
		t.Fatalf("predictions look wrong: M/M/c %.1f, M/G/c %.1f", p.MMcWqMs, p.MGcWqMs)
	}
	// One slot per core was only for the run.
	for _, b := range l.Bal.Backends() {
		if b.maxInflight != 0 {
			t.Fatalf("max_inflight not restored on %s", b.Name)
		}
	}
}

func TestExperimentLimits(t *testing.T) {
	l, _ := experimentCluster(t, 1, 1)
	l.Limits.MaxExperimentS = 60
	if _, err := l.StartExperiment(ExperimentSpec{Kind: "compare", DurationS: 60, Repeats: 5}); err == nil {
		t.Fatal("experiment over the server's time limit was accepted")
	}
	if _, err := l.StartExperiment(ExperimentSpec{Kind: "theory", Utilizations: []float64{1.2}, DurationS: 5}); err == nil {
		t.Fatal("utilization >= 1 was accepted")
	}
}

func TestTheoryFindingIgnoresNoise(t *testing.T) {
	pts := []TheoryPoint{
		{Rho: 0.5, WqMs: 3.1, MGcWqMs: 1.9, MMcWqMs: 3.7, ErrorPct: 67}, // under 5 ms: noise
		{Rho: 0.7, WqMs: 7.7, MGcWqMs: 7.9, MMcWqMs: 15.9, ErrorPct: -3},
		{Rho: 0.9, WqMs: 69.3, MGcWqMs: 69.9, MMcWqMs: 139.9, ErrorPct: -1},
		{Rho: 0.94, WqMs: 143, MGcWqMs: 115, MMcWqMs: 230, ErrorPct: 24},
	}
	got := theoryFinding(pts)
	want := "From 70% to 90% utilization the measured queue wait was within 3% of M/G/c (plain M/M/c predicts 2.0x more, because job times here are nearly constant); at 94% it was 143 ms vs 115 ms predicted (+24%)."
	if got != want {
		t.Fatalf("finding:\n got  %s\n want %s", got, want)
	}
}

func TestSavedExperimentsReloadOnStart(t *testing.T) {
	dir := t.TempDir()
	e := &Experiment{ID: "20260101-000000-abc", Status: "done", Started: time.Now(), Finding: "saved finding", Spec: ExperimentSpec{Kind: "theory"}}
	l := &LB{}
	l.exps.dir = dir
	l.saveExperiment(e)

	var reloaded experiments
	reloaded.dir = dir
	reloaded.load()
	if len(reloaded.list) != 1 || reloaded.list[0].Finding != "saved finding" {
		t.Fatalf("saved experiment not reloaded: %+v", reloaded.list)
	}
}
