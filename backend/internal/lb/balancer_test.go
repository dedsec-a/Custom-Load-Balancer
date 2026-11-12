package lb

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func testBalancer(t *testing.T, algo Algorithm, names ...string) (*Balancer, []*Backend) {
	t.Helper()
	est := NewEstimator()
	est.Seed("/fast", 10*time.Millisecond)
	est.Seed("/slow", 100*time.Millisecond)
	bl := NewBalancer(est, BalancerOptions{Algorithm: algo, QueueTimeout: 2 * time.Second})
	var bs []*Backend
	for _, n := range names {
		b, err := newBackend(n, "http://127.0.0.1:1", http.DefaultTransport)
		if err != nil {
			t.Fatal(err)
		}
		b.healthy.Store(true)
		b.maxInflight = 1
		if err := bl.Add(b); err != nil {
			t.Fatal(err)
		}
		bs = append(bs, b)
	}
	return bl, bs
}

func queueLen(bl *Balancer) int {
	bl.mu.Lock()
	defer bl.mu.Unlock()
	return bl.queue.Len()
}

func waitQueueLen(t *testing.T, bl *Balancer, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for queueLen(bl) != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue length %d, want %d", queueLen(bl), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	bl, _ := testBalancer(t, RoundRobin, "a", "b", "c")
	var got []string
	for range 6 {
		b, _, err := bl.Acquire(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b.Name)
		// RR ignores capacity: don't release, it must still rotate.
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	bl, bs := testBalancer(t, RoundRobin, "a", "b", "c")
	bs[1].healthy.Store(false)
	for range 4 {
		b, _, _ := bl.Acquire(context.Background(), "/x")
		if b.Name == "b" {
			t.Fatal("picked unhealthy backend")
		}
	}
}

// order fills the single slot, queues /slow then /fast, then frees the slot
// once and reports which request ran first.
func order(t *testing.T, algo Algorithm, aging float64) []string {
	t.Helper()
	bl, _ := testBalancer(t, algo, "a")
	bl.SetAging(aging)
	held, _, err := bl.Acquire(context.Background(), "/hold")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 2)
	for i, p := range []string{"/slow", "/fast"} {
		go func() {
			b, _, err := bl.Acquire(context.Background(), p)
			if err != nil {
				results <- "err:" + err.Error()
				return
			}
			results <- p
			time.Sleep(5 * time.Millisecond)
			bl.Release(b)
		}()
		waitQueueLen(t, bl, i+1) // make arrival order deterministic
	}
	bl.Release(held)
	return []string{<-results, <-results}
}

func TestFCFSServesOldestFirst(t *testing.T) {
	if got := order(t, FCFS, 0); got[0] != "/slow" {
		t.Fatalf("FCFS order = %v, want /slow first", got)
	}
}

func TestSJFServesShortestFirst(t *testing.T) {
	if got := order(t, SJF, 0); got[0] != "/fast" {
		t.Fatalf("SJF order = %v, want /fast first", got)
	}
}

func TestSJFAgingPreventsStarvation(t *testing.T) {
	bl, _ := testBalancer(t, SJF, "a")
	bl.SetAging(0.1) // 1 s of waiting = 100 ms shorter
	held, _, _ := bl.Acquire(context.Background(), "/hold")

	// A /slow job that has already waited 2 s must beat a brand-new /fast.
	bl.mu.Lock()
	old := &waiter{path: "/slow", cost: 100, enq: time.Now().Add(-2 * time.Second), ready: make(chan *Backend, 1)}
	fresh := &waiter{path: "/fast", cost: 10, enq: time.Now(), seq: 1, ready: make(chan *Backend, 1)}
	for _, w := range []*waiter{fresh, old} {
		w.key = bl.keyLocked(w)
		pushLocked(bl, w)
	}
	bl.mu.Unlock()

	bl.Release(held)
	select {
	case <-old.ready:
	case <-fresh.ready:
		t.Fatal("fresh short job ran before long job that waited 2 s")
	}
}

func pushLocked(bl *Balancer, w *waiter) {
	bl.queue.Push(w)
	for i := len(bl.queue) - 1; i > 0; {
		p := (i - 1) / 2
		if !bl.queue.Less(i, p) {
			break
		}
		bl.queue.Swap(i, p)
		i = p
	}
}

func TestQueuedPicksLeastLoaded(t *testing.T) {
	bl, bs := testBalancer(t, FCFS, "a", "b")
	for _, b := range bs {
		b.maxInflight = 4
	}
	seen := map[string]int{}
	for range 8 {
		b, _, err := bl.Acquire(context.Background(), "/x")
		if err != nil {
			t.Fatal(err)
		}
		seen[b.Name]++
	}
	if seen["a"] != 4 || seen["b"] != 4 {
		t.Fatalf("distribution = %v, want 4/4", seen)
	}
}

func TestQueueTimeout(t *testing.T) {
	bl, _ := testBalancer(t, FCFS, "a")
	bl.SetQueueLimits(0, 30*time.Millisecond)
	bl.Acquire(context.Background(), "/hold")
	_, _, err := bl.Acquire(context.Background(), "/x")
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err = %v, want ErrQueueTimeout", err)
	}
	if queueLen(bl) != 0 {
		t.Fatal("timed-out request left in queue")
	}
}

func TestCancelWhileQueued(t *testing.T) {
	bl, bs := testBalancer(t, SJF, "a")
	held, _, _ := bl.Acquire(context.Background(), "/hold")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, _, err := bl.Acquire(ctx, "/x"); errc <- err }()
	waitQueueLen(t, bl, 1)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	bl.Release(held)
	if bs[0].inflight != 0 || queueLen(bl) != 0 {
		t.Fatalf("leak: inflight=%d queue=%d", bs[0].inflight, queueLen(bl))
	}
}

func TestNoHealthyBackends(t *testing.T) {
	bl, bs := testBalancer(t, Auto, "a")
	bs[0].healthy.Store(false)
	if _, _, err := bl.Acquire(context.Background(), "/x"); !errors.Is(err, ErrNoBackends) {
		t.Fatalf("err = %v", err)
	}
}

func TestSwitchToRoundRobinDrainsQueue(t *testing.T) {
	bl, _ := testBalancer(t, FCFS, "a")
	bl.Acquire(context.Background(), "/hold")
	done := make(chan error, 1)
	go func() { _, _, err := bl.Acquire(context.Background(), "/x"); done <- err }()
	waitQueueLen(t, bl, 1)
	bl.SetAlgorithm(RoundRobin) // RR has no capacity limit: queued request must go now
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request not released on switch to round robin")
	}
}

func TestAutoMode(t *testing.T) {
	bl, bs := testBalancer(t, Auto, "a")
	bs[0].maxInflight = 10
	bl.est.Observe("/fast", 10*time.Millisecond)
	bl.est.Observe("/slow", 100*time.Millisecond)

	if bl.Snapshot().Effective != RoundRobin {
		t.Fatal("auto should start in round robin")
	}
	for range 9 { // 90% utilization
		bl.Acquire(context.Background(), "/x")
	}
	bl.AutoTick()
	if got := bl.Snapshot().Effective; got != SJF {
		t.Fatalf("high load + mixed job sizes: effective = %s, want sjf", got)
	}
	for range 5 {
		bl.Release(bs[0])
	}
	bl.AutoTick() // 40% utilization, empty queue
	if got := bl.Snapshot().Effective; got != RoundRobin {
		t.Fatalf("low load: effective = %s, want round_robin", got)
	}
}
