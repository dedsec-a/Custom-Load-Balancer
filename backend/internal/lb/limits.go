package lb

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a classic token bucket: `rate` tokens per second up to
// `burst`. Used for per-IP rate limits and for the retry budget.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(now time.Time, rate, burst, cost float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// rateLimiter keeps one token bucket per key (a client IP).
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*tokenBucket
}

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	if perSecond <= 0 {
		return nil
	}
	return &rateLimiter{rate: perSecond, burst: burst, buckets: map[string]*tokenBucket{}}
}

func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 50_000 { // bound memory: forget clients idle for a minute
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[key]
	if b == nil {
		b = &tokenBucket{}
		l.buckets[key] = b
	}
	return b.take(now, l.rate, l.burst, 1)
}

// clientIP is the address to rate-limit. Behind Caddy, X-Forwarded-For
// holds the real client (Caddy overwrites whatever an untrusted client
// sends), so with trustXFF its first entry is used; otherwise the TCP peer.
func clientIP(r *http.Request, trustXFF bool) string {
	if trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fromLoopbackDirect = our own load generator (or a local tool): exempt
// from the public rate limit.
func fromLoopbackDirect(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// retryBudget allows retries worth at most `ratio` of recent requests
// (plus a small burst), so a sick cluster isn't hit with a retry storm.
type retryBudget struct {
	mu     sync.Mutex
	tokens float64
	ratio  float64
	cap    float64
}

func newRetryBudget(ratio, burst float64) *retryBudget {
	return &retryBudget{tokens: burst, ratio: ratio, cap: burst}
}

func (b *retryBudget) deposit() {
	b.mu.Lock()
	b.tokens = min(b.cap, b.tokens+b.ratio)
	b.mu.Unlock()
}

func (b *retryBudget) withdraw() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
