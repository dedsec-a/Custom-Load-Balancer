package lb

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"loadbalancer/internal/backend"
)

// Backend is one upstream server as the load balancer sees it.
type Backend struct {
	Name  string
	URL   *url.URL
	proxy *httputil.ReverseProxy

	healthy atomic.Bool
	fails   atomic.Int32                  // consecutive failed health polls (active check)
	stats   atomic.Pointer[backend.Stats] // last /stats poll
	served  atomic.Uint64
	failed  atomic.Uint64 // proxy errors (502s)

	draining     atomic.Bool  // no new requests; removed when in-flight reaches 0
	passiveFails atomic.Int32 // consecutive failed real requests (passive check)
	ejectedUntil atomic.Int64 // unix ns; passive ejection cool-down
	ejections    atomic.Uint64
	faultUntil   atomic.Int64 // unix ns; simulated outage set through the LB

	// Guarded by Balancer.mu.
	inflight    int
	maxInflight int // 0 = auto (cores + db_pool)
}

func newBackend(name, rawURL string, transport http.RoundTripper) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("backend %q: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend %q: url must look like http://host:port, got %q", name, rawURL)
	}
	if name == "" {
		return nil, fmt.Errorf("backend name is required")
	}
	b := &Backend{Name: name, URL: u}
	b.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
		},
		Transport: transport,
		// An upstream 502/503/504 on a retryable request becomes an error, so
		// nothing is written yet and ServeHTTP can try another backend.
		ModifyResponse: func(resp *http.Response) error {
			switch resp.StatusCode {
			case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				if retryable(resp.Request) {
					return &upstreamStatusError{status: resp.StatusCode}
				}
			}
			return nil
		},
		// Record the failure for ServeHTTP instead of answering the client.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			b.failed.Add(1)
			if a, ok := r.Context().Value(attemptKey{}).(*attempt); ok {
				a.err = err
				return
			}
			http.Error(w, "bad gateway: "+b.Name, http.StatusBadGateway)
		},
	}
	return b, nil
}

// attempt carries one proxy attempt's failure back to ServeHTTP.
type attempt struct{ err error }
type attemptKey struct{}

// upstreamStatusError = the backend answered 502/503/504.
type upstreamStatusError struct{ status int }

func (e *upstreamStatusError) Error() string { return fmt.Sprintf("backend answered %d", e.status) }

// retryable: only requests that are safe to send twice and have no body.
func retryable(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.Body == nil || r.Body == http.NoBody)
}

// usable = may receive new requests: healthy and not being drained.
func (b *Backend) usable() bool { return b.healthy.Load() && !b.draining.Load() }

func untilNow(ns int64) time.Duration {
	if ns == 0 {
		return 0
	}
	return max(time.Until(time.Unix(0, ns)), 0)
}

func (b *Backend) ejectedFor() time.Duration { return untilNow(b.ejectedUntil.Load()) }
func (b *Backend) faultFor() time.Duration   { return untilNow(b.faultUntil.Load()) }

// capacity is how many requests the LB lets this backend hold at once in
// the queued algorithms (FCFS / SJF). Extra requests wait in the LB queue,
// where the algorithm decides who goes next.
func (b *Backend) capacity() int {
	if b.maxInflight > 0 {
		return b.maxInflight
	}
	if s := b.stats.Load(); s != nil && s.Cores+s.DBPool > 0 {
		return s.Cores + s.DBPool
	}
	return 8
}
