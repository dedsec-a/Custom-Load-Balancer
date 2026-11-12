//go:build perf

// Performance gate, run in CI with: go test -tags perf -run TestPerf ./internal/lb
// Budgets are set well below what a laptop measures (see README), so a
// slower CI runner passes but a real regression fails.
package lb

import (
	"testing"
	"time"
)

func runPerf(t *testing.T, spec ExperimentSpec) *Experiment {
	t.Helper()
	l, _ := experimentCluster(t, 2, 5) // 2 backends x (2 cores, 5 DB connections)
	e, err := l.StartExperiment(spec)
	if err != nil {
		t.Fatal(err)
	}
	done := waitExperiment(t, l, e.ID, 3*time.Minute)
	if done.Status != "done" {
		t.Fatalf("experiment %s: %s", done.Status, done.Error)
	}
	return done
}

func TestPerfBaselineLatency(t *testing.T) {
	// 2 backends x (2 cores, 5 DB): /browse is DB-bound at about 10 conns / 15 ms = 660 req/s.
	done := runPerf(t, ExperimentSpec{Kind: "compare", Users: 25, Mix: map[string]int{"/browse": 1},
		DurationS: 10, WarmupS: 2, Repeats: 1, Variants: []Variant{{Name: "FCFS", Algorithm: FCFS}}})
	s := done.Summary[0]
	p95, tp := s.Paths["/browse"].P95.Median, s.Throughput.Median
	t.Logf("/browse at 25 users: p95 %.1f ms, %.0f req/s", p95, tp)
	if p95 > 150 {
		t.Errorf("p95 %.1f ms over the 150 ms budget", p95)
	}
	if tp < 350 {
		t.Errorf("throughput %.0f req/s under the 350 req/s floor", tp)
	}
}

func TestPerfSJFHelpsShortJobs(t *testing.T) {
	aging := 0.02
	done := runPerf(t, ExperimentSpec{Kind: "compare", Users: 100, DurationS: 12, WarmupS: 3, Repeats: 1,
		Variants: []Variant{{Name: "FCFS", Algorithm: FCFS}, {Name: "SJF", Algorithm: SJF, Aging: &aging}}})
	fcfs, sjf := done.Summary[0].Paths["/browse"].P95.Median, done.Summary[1].Paths["/browse"].P95.Median
	t.Logf("/browse p95 under overload: FCFS %.0f ms, SJF %.0f ms (%s)", fcfs, sjf, done.Finding)
	if sjf > 0.6*fcfs {
		t.Errorf("SJF should cut /browse p95 to at most 60%% of FCFS: got %.0f vs %.0f ms", sjf, fcfs)
	}
}
