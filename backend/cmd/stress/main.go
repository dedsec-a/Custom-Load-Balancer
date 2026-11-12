// Command stress ramps up concurrent clients against a URL and reports, per
// step, throughput, latency percentiles and errors. It stops at the first
// step that breaks the SLO and prints the highest load that still met it.
//
//	go run ./cmd/stress -url http://127.0.0.1:8080/browse
//	go run ./cmd/stress -url http://127.0.0.1:8080/ -steps 50,100,200,400,800 -slo-p95 50ms
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type result struct {
	conc     int
	requests int
	errors   int
	rps      float64
	p50      time.Duration
	p90      time.Duration
	p95      time.Duration
	p99      time.Duration
	max      time.Duration
	errRate  float64
	pass     bool
	why      string
	kinds    map[string]int
}

// errKind turns an error into a short, groupable label.
func errKind(err error, resp *http.Response) string {
	if err == nil {
		return "HTTP " + resp.Status
	}
	msg := err.Error()
	for _, k := range []string{"connectex", "wsarecv", "wsasend", "connection reset", "connection refused", "Client.Timeout", "EOF", "context deadline"} {
		if strings.Contains(msg, k) {
			if i := strings.LastIndex(msg, ": "); i >= 0 {
				return msg[i+2:]
			}
			return k
		}
	}
	return msg
}

func main() {
	target := flag.String("url", "http://127.0.0.1:8080/browse", "URL to hit (through the load balancer)")
	stepsF := flag.String("steps", "10,25,50,100,200,400,800", "comma-separated concurrency levels")
	dur := flag.Duration("duration", 10*time.Second, "measured time per step")
	warm := flag.Duration("warmup", 2*time.Second, "time per step before measuring starts")
	slo := flag.Duration("slo-p95", 500*time.Millisecond, "p95 latency target")
	maxErr := flag.Float64("max-error-rate", 0.01, "error-rate target (0.01 = 1%)")
	timeout := flag.Duration("timeout", 15*time.Second, "per-request timeout")
	keepGoing := flag.Bool("keep-going", false, "run every step even after the SLO breaks")
	csvPath := flag.String("csv", "", "also write results to this CSV file")
	ramp := flag.Bool("ramp", true, "spread client starts over the warm-up (false = all connect at once)")
	flag.Parse()

	var steps []int
	for s := range strings.SplitSeq(*stepsF, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			log.Fatalf("bad step %q", s)
		}
		steps = append(steps, n)
	}
	top := slices.Max(steps)
	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        top,
			MaxIdleConnsPerHost: top,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
	}

	fmt.Printf("Target %s · %v per step (+%v warm-up) · SLO p95 <= %v, errors <= %.1f%%\n\n",
		*target, *dur, *warm, *slo, *maxErr*100)
	fmt.Printf("%6s %9s %8s %9s %9s %9s %9s %9s %7s  %s\n",
		"users", "requests", "req/s", "p50", "p90", "p95", "p99", "max", "errors", "verdict")

	var results []result
	for _, n := range steps {
		r := runStep(client, *target, n, *warm, *dur, *ramp)
		switch {
		case r.requests == 0:
			r.why = "no responses"
		case r.errRate > *maxErr:
			r.why = fmt.Sprintf("errors %.1f%%", r.errRate*100)
		case r.p95 > *slo:
			r.why = fmt.Sprintf("p95 %v > %v", r.p95.Round(time.Millisecond), *slo)
		default:
			r.pass = true
		}
		results = append(results, r)
		verdict := "ok"
		if !r.pass {
			verdict = "FAIL: " + r.why
		}
		fmt.Printf("%6d %9d %8.0f %9s %9s %9s %9s %9s %6.1f%%  %s\n",
			r.conc, r.requests, r.rps, ms(r.p50), ms(r.p90), ms(r.p95), ms(r.p99), ms(r.max), r.errRate*100, verdict)
		for k, v := range r.kinds {
			fmt.Printf("%8s└ %d × %s\n", "", v, k)
		}
		if !r.pass && !*keepGoing {
			break
		}
	}

	fmt.Println()
	var best *result
	peak := results[0]
	for i := range results {
		if results[i].pass && (best == nil || results[i].rps > best.rps) {
			best = &results[i]
		}
		if results[i].rps > peak.rps {
			peak = results[i]
		}
	}
	if best != nil {
		fmt.Printf("Max load within SLO: %.0f req/s at %d concurrent users (p95 %s)\n", best.rps, best.conc, ms(best.p95))
	} else {
		fmt.Println("No step met the SLO. Start with fewer users or relax -slo-p95.")
	}
	fmt.Printf("Peak throughput seen: %.0f req/s at %d users\n", peak.rps, peak.conc)
	if last := results[len(results)-1]; !last.pass {
		fmt.Printf("Breaking point: %d users (%s)\n", last.conc, last.why)
	} else {
		fmt.Println("Never broke the SLO. Add higher -steps to find the limit.")
	}

	if *csvPath != "" {
		if err := writeCSV(*csvPath, results); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Saved", *csvPath)
	}
}

// runStep runs n closed-loop clients (send, wait for reply, repeat).
func runStep(client *http.Client, url string, n int, warm, dur time.Duration, ramp bool) result {
	ctx, cancel := context.WithTimeout(context.Background(), warm+dur)
	defer cancel()
	measureFrom := time.Now().Add(warm)

	type perWorker struct {
		lat    []time.Duration
		errors int
		kinds  map[string]int
	}
	all := make([]perWorker, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			w := &all[i]
			// Spread client starts over the warm-up. Opening hundreds of
			// connections in the same instant overflows the OS accept backlog
			// (~200 on Windows) and gets "connection refused", which is a
			// connect storm, not load. Real users never arrive that way.
			if ramp {
				select {
				case <-time.After(warm * time.Duration(i) / time.Duration(n)):
				case <-ctx.Done():
					return
				}
			}
			for ctx.Err() == nil {
				start := time.Now()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				resp, err := client.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				if ctx.Err() != nil || start.Before(measureFrom) {
					continue // step ended mid-request, or still warming up
				}
				if err != nil || resp.StatusCode >= 400 {
					w.errors++
					if w.kinds == nil {
						w.kinds = map[string]int{}
					}
					w.kinds[errKind(err, resp)]++
					if err != nil {
						time.Sleep(10 * time.Millisecond) // don't spin on a dead target
					}
					continue
				}
				w.lat = append(w.lat, time.Since(start))
			}
		})
	}
	wg.Wait()

	var lat []time.Duration
	errs := 0
	kinds := map[string]int{}
	for _, w := range all {
		lat = append(lat, w.lat...)
		errs += w.errors
		for k, v := range w.kinds {
			kinds[k] += v
		}
	}
	slices.Sort(lat)
	r := result{conc: n, requests: len(lat) + errs, errors: errs, kinds: kinds}
	r.rps = float64(len(lat)) / dur.Seconds()
	if r.requests > 0 {
		r.errRate = float64(errs) / float64(r.requests)
	}
	if len(lat) > 0 {
		at := func(q float64) time.Duration { return lat[min(len(lat)-1, int(q*float64(len(lat))))] }
		r.p50, r.p90, r.p95, r.p99, r.max = at(0.50), at(0.90), at(0.95), at(0.99), lat[len(lat)-1]
	}
	return r
}

func ms(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fms", float64(d)/1e6)
}

func writeCSV(path string, rs []result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"users", "requests", "rps", "p50_ms", "p90_ms", "p95_ms", "p99_ms", "max_ms", "error_rate", "pass"})
	f2 := func(d time.Duration) string { return strconv.FormatFloat(float64(d)/1e6, 'f', 2, 64) }
	for _, r := range rs {
		w.Write([]string{strconv.Itoa(r.conc), strconv.Itoa(r.requests), strconv.FormatFloat(r.rps, 'f', 1, 64),
			f2(r.p50), f2(r.p90), f2(r.p95), f2(r.p99), f2(r.max), strconv.FormatFloat(r.errRate, 'f', 4, 64), strconv.FormatBool(r.pass)})
	}
	w.Flush()
	return w.Error()
}
