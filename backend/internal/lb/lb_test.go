package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"loadbalancer/internal/backend"
)

// cluster starts n real simulated backends and an LB in front of them.
func cluster(t *testing.T, n int, algo Algorithm) (*LB, *httptest.Server, *httptest.Server) {
	t.Helper()
	var specs []BackendSpec
	for i := range n {
		name := string(rune('a' + i))
		srv := httptest.NewServer(backend.New(name, 2, 4).Handler())
		t.Cleanup(srv.Close)
		specs = append(specs, BackendSpec{Name: name, URL: srv.URL})
	}
	l, err := New(Config{Backends: specs, Algorithm: algo})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l.pollAll(ctx)
	front := httptest.NewServer(l)
	t.Cleanup(front.Close)
	admin := httptest.NewServer(l.AdminHandler("*", "", ""))
	t.Cleanup(admin.Close)
	return l, front, admin
}

func TestProxyRoundRobinEndToEnd(t *testing.T) {
	_, front, _ := cluster(t, 3, RoundRobin)
	counts := map[string]int{}
	for range 9 {
		resp, err := http.Get(front.URL + "/browse")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		counts[resp.Header.Get("X-LB-Backend")]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if counts[n] != 3 {
			t.Fatalf("distribution %v, want 3 each", counts)
		}
	}
}

func TestProxyUnderLoadSJF(t *testing.T) {
	l, front, _ := cluster(t, 2, SJF)
	var wg sync.WaitGroup
	errs := make(chan int, 200)
	for i := range 120 {
		path := "/browse"
		if i%4 == 0 {
			path = "/search"
		}
		wg.Go(func() {
			resp, err := http.Get(front.URL + path)
			if err != nil {
				errs <- -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- resp.StatusCode
			}
		})
	}
	wg.Wait()
	close(errs)
	for code := range errs {
		t.Fatalf("request failed: %d", code)
	}
	est := l.Est.Snapshot()
	if est["/search"].EstMs <= est["/browse"].EstMs {
		t.Fatalf("estimator should learn /search is longer: %+v", est)
	}
	st := l.Bal.Snapshot()
	for _, b := range st.Backends {
		if b.Inflight != 0 {
			t.Fatalf("inflight leak on %s: %d", b.Name, b.Inflight)
		}
	}
}

func doJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestAdminScaleResources(t *testing.T) {
	_, _, admin := cluster(t, 2, Auto)
	var views []BackendView
	if code := doJSON(t, "PUT", admin.URL+"/api/resources", map[string]int{"cores": 8, "db_pool": 30}, &views); code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, v := range views {
		if v.Cores != 8 || v.DBPool != 30 || v.Capacity != 38 {
			t.Fatalf("backend not scaled: %+v", v)
		}
	}
	if code := doJSON(t, "PUT", admin.URL+"/api/backends/a/resources", map[string]int{"cores": 0}, nil); code != 400 {
		t.Fatalf("cores=0 should be rejected, got %d", code)
	}
	if code := doJSON(t, "PUT", admin.URL+"/api/backends/zzz/resources", map[string]int{"cores": 2}, nil); code != 404 {
		t.Fatalf("unknown backend should 404, got %d", code)
	}
}

func TestAdminAlgorithmAndLoad(t *testing.T) {
	l, front, admin := cluster(t, 2, Auto)
	l.Load = NewLoadGen(front.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Load.Start(ctx)

	var st State
	if code := doJSON(t, "PUT", admin.URL+"/api/algorithm", map[string]any{"algorithm": "fcfs"}, &st); code != 200 || st.Effective != FCFS {
		t.Fatalf("set algorithm: code=%d effective=%s", code, st.Effective)
	}
	if code := doJSON(t, "PUT", admin.URL+"/api/algorithm", map[string]any{"algorithm": "nope"}, nil); code != 400 {
		t.Fatalf("bad algorithm should 400, got %d", code)
	}

	var lc LoadConfig
	if code := doJSON(t, "PUT", admin.URL+"/api/load", map[string]any{"users": 20}, &lc); code != 200 || lc.Users != 20 {
		t.Fatalf("set load: code=%d cfg=%+v", code, lc)
	}
	time.Sleep(1200 * time.Millisecond)
	p := l.Metrics.Flush(time.Now(), l.Bal.Snapshot(), l.Est.Snapshot(), l.Load.Users())
	if p.Requests == 0 || p.Users != 20 || p.P95Ms <= 0 {
		t.Fatalf("expected traffic in metrics, got %+v", p)
	}
	doJSON(t, "PUT", admin.URL+"/api/load", map[string]any{"users": 0}, nil)
	if l.Load.Users() != 0 {
		t.Fatal("users not stopped")
	}

	var hist []Point
	doJSON(t, "GET", admin.URL+"/api/metrics?last=5", nil, &hist)
	if len(hist) == 0 {
		t.Fatal("no metrics history")
	}
}

func TestServerTiming(t *testing.T) {
	cases := []struct {
		h    string
		want time.Duration
		ok   bool
	}{
		{"work;dur=12.5, wait;dur=3", 12500 * time.Microsecond, true},
		{"wait;dur=3, work;desc=\"x\";dur=7", 7 * time.Millisecond, true},
		{"wait;dur=3", 0, false},
		{"work;dur=abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := serverTiming(c.h, "work")
		if got != c.want || ok != c.ok {
			t.Errorf("serverTiming(%q) = %v,%v want %v,%v", c.h, got, ok, c.want, c.ok)
		}
	}
}

func TestDemoLimits(t *testing.T) {
	l, _, _ := cluster(t, 2, Auto)
	l.Limits = Limits{MaxUsers: 50, MaxCores: 4, MaxDBPool: 10, LockBackends: true}
	admin := httptest.NewServer(l.AdminHandler("", "", ""))
	defer admin.Close()

	if code := doJSON(t, "PUT", admin.URL+"/api/load", map[string]int{"users": 51}, nil); code != 400 {
		t.Fatalf("users over limit: got %d, want 400", code)
	}
	if code := doJSON(t, "PUT", admin.URL+"/api/load", map[string]int{"users": 50}, nil); code != 200 {
		t.Fatalf("users at limit: got %d, want 200", code)
	}
	doJSON(t, "PUT", admin.URL+"/api/load", map[string]int{"users": 0}, nil)
	if code := doJSON(t, "PUT", admin.URL+"/api/resources", map[string]int{"cores": 5}, nil); code != 400 {
		t.Fatalf("cores over limit: got %d, want 400", code)
	}
	// SSRF guard: no pointing the LB at arbitrary URLs.
	if code := doJSON(t, "POST", admin.URL+"/api/backends", BackendSpec{Name: "x", URL: "http://169.254.169.254"}, nil); code != 403 {
		t.Fatalf("add backend when locked: got %d, want 403", code)
	}
	if code := doJSON(t, "DELETE", admin.URL+"/api/backends/a", nil, nil); code != 403 {
		t.Fatalf("remove backend when locked: got %d, want 403", code)
	}
	var st State
	doJSON(t, "GET", admin.URL+"/api/state", nil, &st)
	if st.Limits.MaxUsers != 50 || !st.Limits.LockBackends {
		t.Fatalf("limits not reported in state: %+v", st.Limits)
	}
}

func TestIdleStop(t *testing.T) {
	l, front, _ := cluster(t, 1, Auto)
	l.Load = NewLoadGen(front.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Load.Start(ctx)
	l.Limits.IdleStop = time.Minute
	l.Load.Set(LoadConfig{Users: 3, Mix: map[string]int{"/browse": 1}})
	l.touched()

	l.idleStop(time.Now())
	if l.Load.Users() != 3 {
		t.Fatal("stopped users before the idle period ended")
	}
	l.idleStop(time.Now().Add(2 * time.Minute))
	if l.Load.Users() != 0 {
		t.Fatal("users still running after idle period")
	}
}

func TestBasicAuthAndUI(t *testing.T) {
	l, _, _ := cluster(t, 1, Auto)
	dir := t.TempDir()
	os.WriteFile(dir+"/index.html", []byte("<h1>dashboard</h1>"), 0o644)
	admin := httptest.NewServer(l.AdminHandler("", dir, "s3cret"))
	defer admin.Close()

	get := func(path, pass string) (int, string) {
		req, _ := http.NewRequest("GET", admin.URL+path, nil)
		if pass != "" {
			req.SetBasicAuth("admin", pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/api/state", ""); code != 401 {
		t.Fatalf("no password: got %d, want 401", code)
	}
	if code, _ := get("/api/state", "wrong"); code != 401 {
		t.Fatalf("wrong password: got %d, want 401", code)
	}
	if code, _ := get("/healthz", ""); code != 200 {
		t.Fatalf("healthz must stay open for health checks, got %d", code)
	}
	if code, body := get("/some/client/route", "s3cret"); code != 200 || body != "<h1>dashboard</h1>" {
		t.Fatalf("SPA fallback: %d %q", code, body)
	}
	if code, _ := get("/api/state", "s3cret"); code != 200 {
		t.Fatalf("right password: got %d", code)
	}
}

func TestPrometheusMetrics(t *testing.T) {
	l, front, _ := cluster(t, 2, RoundRobin)
	for range 5 {
		resp, err := http.Get(front.URL + "/browse")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	rec := httptest.NewRecorder()
	l.ServeMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`lb_requests_total{path="/browse",outcome="ok"} 5`,
		`lb_request_duration_seconds_bucket{path="/browse",le="+Inf"} 5`,
		`lb_request_duration_seconds_count{path="/browse"} 5`,
		`lb_backend_up{backend="a"} 1`,
		`lb_algorithm_info{configured="round_robin",effective="round_robin"} 1`,
		"# TYPE lb_queue_wait_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// Buckets must be cumulative (non-decreasing).
	prev := -1
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, `lb_request_duration_seconds_bucket{path="/browse"`) {
			var n int
			fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &n)
			if n < prev {
				t.Fatalf("bucket counts decrease: %s", line)
			}
			prev = n
		}
	}
}
