package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"loadbalancer/internal/lb"
)

// parseBackends accepts "b1=http://host:3001,b2=http://host:3002"
// or just "http://host:3001,http://host:3002" (named b1, b2, ...).
func parseBackends(s string) ([]lb.BackendSpec, error) {
	var out []lb.BackendSpec
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok {
			name, url = fmt.Sprintf("b%d", i+1), part
		}
		out = append(out, lb.BackendSpec{Name: strings.TrimSpace(name), URL: strings.TrimSpace(url)})
	}
	if len(out) == 0 {
		return nil, errors.New("no backends given")
	}
	return out, nil
}

func main() {
	port := flag.String("port", "8080", "public port (traffic)")
	adminPort := flag.String("admin-port", "8081", "control-plane port (front-end API); keep private")
	backends := flag.String("backends", "b1=http://127.0.0.1:3001,b2=http://127.0.0.1:3002,b3=http://127.0.0.1:3003", "comma-separated name=url list")
	algo := flag.String("algorithm", "auto", "round_robin | least_conn | fcfs | sjf | auto")
	aging := flag.Float64("aging", 0.02, "SJF aging: ms of job cost forgiven per ms waited (0 = pure SJF; higher = fairer to long jobs)")
	maxQueue := flag.Int("max-queue", 10000, "max requests waiting in the LB queue")
	queueTimeout := flag.Duration("queue-timeout", 10*time.Second, "max time a request may wait in the LB queue")
	cors := flag.String("cors-origin", "*", "Access-Control-Allow-Origin for the admin API (empty to disable)")
	uiDir := flag.String("ui-dir", "", "serve the built dashboard from this folder on the admin port")
	demo := flag.Bool("demo", false, "public-demo safety limits: cap users/cores/DB pool, lock backend list, stop idle load")
	maxUsers := flag.Int("max-users", 300, "with -demo: most virtual users allowed")
	maxCores := flag.Int("max-cores", 8, "with -demo: most cores per backend")
	maxDB := flag.Int("max-db-pool", 50, "with -demo: largest DB pool per backend")
	idleStop := flag.Duration("idle-stop", 10*time.Minute, "with -demo: stop virtual users after this long without a change")
	maxFault := flag.Int("max-fault", 120, "with -demo: longest simulated backend outage, in seconds")
	upstreamTimeout := flag.Duration("upstream-timeout", 15*time.Second, "max wait for a backend's response headers")
	trustXFF := flag.Bool("trust-xff", false, "behind a trusted reverse proxy (Caddy): rate-limit by X-Forwarded-For")
	adminRate := flag.Float64("admin-rate", 5, "admin changes per second per IP (0 = unlimited)")
	publicRate := flag.Float64("public-rate", 0, "proxied requests per second per IP (0 = unlimited; the built-in load generator is exempt)")
	logFormat := flag.String("log", "json", "log format: json | text")
	resultsDir := flag.String("results-dir", "", "save finished experiments here as JSON (empty = keep in memory only)")
	maxExperiment := flag.Duration("max-experiment", 30*time.Minute, "with -demo: longest experiment a visitor may start")
	flag.Parse()

	var h slog.Handler = slog.NewJSONHandler(os.Stderr, nil)
	if *logFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(h))

	var limits lb.Limits
	if *demo {
		limits = lb.Limits{MaxUsers: *maxUsers, MaxCores: *maxCores, MaxDBPool: *maxDB, LockBackends: true, IdleStop: *idleStop, MaxFaultS: *maxFault,
			MaxExperimentS: int(maxExperiment.Seconds())}
	}
	// From the environment, not a flag, so it never shows up in `ps` output.
	password := os.Getenv("LB_ADMIN_PASSWORD")

	specs, err := parseBackends(*backends)
	if err != nil {
		log.Fatal(err)
	}
	a, err := lb.ParseAlgorithm(*algo)
	if err != nil {
		log.Fatal(err)
	}

	l, err := lb.New(lb.Config{
		Backends:     specs,
		Algorithm:    a,
		Aging:        *aging,
		MaxQueue:     *maxQueue,
		QueueTimeout: *queueTimeout,
		LoadTarget:   "http://127.0.0.1:" + *port,
		Limits:       limits,

		UpstreamTimeout: *upstreamTimeout,
		TrustXFF:        *trustXFF,
		AdminRate:       *adminRate,
		PublicRate:      *publicRate,
		ResultsDir:      *resultsDir,
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	public := &http.Server{Addr: ":" + *port, Handler: l, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	admin := &http.Server{Addr: ":" + *adminPort, Handler: l.AdminHandler(*cors, *uiDir, password), ReadHeaderTimeout: 5 * time.Second}

	for _, srv := range []*http.Server{public, admin} {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("listen %s: %v", srv.Addr, err)
			}
		}()
	}
	slog.Info("load balancer started", "port", *port, "admin_port", *adminPort, "algorithm", string(a),
		"backends", len(specs), "demo", *demo, "password", password != "", "ui", *uiDir, "public_rate", *publicRate)

	go l.Run(ctx)
	<-ctx.Done()

	log.Print("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admin.Shutdown(shutCtx)
	public.Shutdown(shutCtx)
}
