package lb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"loadbalancer/internal/backend"
)

// clusterWith starts n backends (cores, dbPool each) and an LB with cfg.
func clusterWith(t *testing.T, n, cores, dbPool int, cfg Config) (*LB, *httptest.Server) {
	t.Helper()
	for i := range n {
		name := string(rune('a' + i))
		srv := httptest.NewServer(backend.New(name, cores, dbPool).Handler())
		t.Cleanup(srv.Close)
		cfg.Backends = append(cfg.Backends, BackendSpec{Name: name, URL: srv.URL})
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.pollAll(context.Background())
	front := httptest.NewServer(l)
	t.Cleanup(front.Close)
	return l, front
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-LB-Backend")
}

func TestLeastConnPicksLeastLoadedAndRotatesTies(t *testing.T) {
	bl, bs := testBalancer(t, LeastConn, "a", "b", "c")
	for _, b := range bs {
		b.maxInflight = 10
	}
	// Idle cluster: ties rotate instead of always picking "a".
	seen := map[string]bool{}
	for range 3 {
		b, _, _ := bl.Acquire(context.Background(), "/x")
		seen[b.Name] = true
		bl.Release(b)
	}
	if len(seen) != 3 {
		t.Fatalf("idle ties should rotate across all backends, got %v", seen)
	}
	// Load a and b: the next pick must be c.
	bs[0].inflight, bs[1].inflight = 5, 3
	b, _, _ := bl.Acquire(context.Background(), "/x")
	if b.Name != "c" {
		t.Fatalf("picked %s, want the least loaded c", b.Name)
	}
	// Least-conn never queues, even past slot limits.
	bs[2].inflight = 50
	if _, wait, err := bl.Acquire(context.Background(), "/x"); err != nil || wait != 0 {
		t.Fatalf("least_conn must forward immediately: wait=%v err=%v", wait, err)
	}
}

func TestFailoverRetriesAndPassiveEjection(t *testing.T) {
	l, front := clusterWith(t, 3, 4, 10, Config{Algorithm: RoundRobin})
	b := l.Bal.Get("b")
	if err := l.SetFault(context.Background(), b, 60); err != nil {
		t.Fatal(err)
	}
	// No health check runs here, so only passive checks + retries can save
	// requests that round robin sends to the dead backend.
	for i := range 30 {
		code, served := get(t, front.URL+"/browse", nil)
		if code != 200 {
			t.Fatalf("request %d: status %d (should have been retried elsewhere)", i, code)
		}
		if served == "b" {
			t.Fatalf("request %d was answered by the downed backend", i)
		}
	}
	p := l.Metrics.Flush(time.Now(), l.Bal.Snapshot(), l.Est.Snapshot(), 0)
	if p.Retries == 0 || p.Errors != 0 {
		t.Fatalf("want retries > 0 and no errors, got retries=%d errors=%d", p.Retries, p.Errors)
	}
	v := l.Bal.Snapshot().Backends[1]
	if !v.Ejected || v.Healthy || v.Ejections != 1 || v.FaultForS <= 0 {
		t.Fatalf("b should be ejected once and marked down: %+v", v)
	}
	// Recover: the backend comes back on the next good check.
	if err := l.SetFault(context.Background(), b, 0); err != nil {
		t.Fatal(err)
	}
	l.pollAll(context.Background())
	if !l.Bal.Get("b").healthy.Load() {
		t.Fatal("b should be healthy again after recovery")
	}
}

func TestPostBodiesAreNotRetried(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.Body = io.NopCloser(nil)
	if retryable(r) {
		t.Fatal("POST must never be retried")
	}
	if !retryable(httptest.NewRequest("GET", "/x", nil)) {
		t.Fatal("GET without a body should be retryable")
	}
}

func TestDrainWaitsForInflight(t *testing.T) {
	bl, bs := testBalancer(t, RoundRobin, "a", "b")
	held, _, _ := bl.Acquire(context.Background(), "/x") // goes to a
	if held.Name != "a" {
		t.Fatalf("setup: got %s", held.Name)
	}
	bl.Drain("a")
	if len(bl.Backends()) != 2 {
		t.Fatal("a removed while a request was still in flight")
	}
	for range 4 {
		b, _, _ := bl.Acquire(context.Background(), "/x")
		if b == bs[0] {
			t.Fatal("draining backend received a new request")
		}
		bl.Release(b)
	}
	bl.Release(held)
	if len(bl.Backends()) != 1 || bl.Backends()[0].Name != "b" {
		t.Fatalf("a should be gone once idle, have %d backends", len(bl.Backends()))
	}
}

func TestLoadSheddingIsCountedNotTimed(t *testing.T) {
	l, front := clusterWith(t, 1, 1, 1, Config{Algorithm: FCFS, MaxQueue: 2, QueueTimeout: 5 * time.Second})
	l.Bal.SetMaxInflight(l.Bal.Get("a"), 1)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for range 20 {
		wg.Go(func() {
			code, _ := get(t, front.URL+"/search", nil)
			codes <- code
		})
	}
	wg.Wait()
	close(codes)
	shed := 0
	for c := range codes {
		if c == 503 {
			shed++
		}
	}
	p := l.Metrics.Flush(time.Now(), l.Bal.Snapshot(), l.Est.Snapshot(), 0)
	if shed == 0 || p.Shed != shed {
		t.Fatalf("want shed requests counted: client saw %d, metrics %d", shed, p.Shed)
	}
	if p.Requests != 20-shed || p.P50Ms < 90 {
		// /search takes 100 ms; shed requests (microseconds) must not drag p50 down.
		t.Fatalf("latency must cover successful requests only: ok=%d p50=%.1f", p.Requests, p.P50Ms)
	}
}

func TestPublicRateLimitByForwardedIP(t *testing.T) {
	_, front := clusterWith(t, 1, 4, 10, Config{Algorithm: RoundRobin, TrustXFF: true, PublicRate: 1})
	codes := []int{}
	for range 5 {
		code, _ := get(t, front.URL+"/", map[string]string{"X-Forwarded-For": "203.0.113.7"})
		codes = append(codes, code)
	}
	if codes[0] != 200 || codes[4] != 429 {
		t.Fatalf("burst of 2 then 429 expected, got %v", codes)
	}
	// Another client has its own bucket.
	if code, _ := get(t, front.URL+"/", map[string]string{"X-Forwarded-For": "198.51.100.9"}); code != 200 {
		t.Fatalf("second client limited by the first one's traffic: %d", code)
	}
	// Our own load generator (loopback, no X-Forwarded-For) is exempt.
	for range 10 {
		if code, _ := get(t, front.URL+"/", nil); code != 200 {
			t.Fatalf("loopback traffic was rate limited: %d", code)
		}
	}
}

func TestAdminWritesAreRateLimitedAndFaultCapped(t *testing.T) {
	l, _ := clusterWith(t, 1, 2, 4, Config{Algorithm: Auto, AdminRate: 1, Limits: Limits{MaxFaultS: 30}})
	admin := httptest.NewServer(l.AdminHandler("", "", ""))
	defer admin.Close()

	if code := doJSON(t, "PUT", admin.URL+"/api/backends/a/fault", map[string]int{"down_for_s": 31}, nil); code != 400 {
		t.Fatalf("outage over the cap: got %d, want 400", code)
	}
	limited := false
	for range 15 {
		if doJSON(t, "PUT", admin.URL+"/api/algorithm", map[string]string{"algorithm": "fcfs"}, nil) == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("admin writes were never rate limited")
	}
	// Reads are never limited.
	if code := doJSON(t, "GET", admin.URL+"/api/state", nil, nil); code != 200 {
		t.Fatalf("reads must not be limited, got %d", code)
	}
}
