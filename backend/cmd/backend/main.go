package main

import (
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"loadbalancer/internal/backend"
)

func main() {
	port := flag.String("port", "3002", "port to listen on")
	name := flag.String("name", "b1", "name of this backend")
	cores := flag.Int("cores", 4, "number of simulated CPU cores")
	dbPool := flag.Int("db-pool", 10, "number of DB connections")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	s := backend.New(*name, *cores, *dbPool)
	srv := &http.Server{
		Addr:              ":" + *port,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	slog.Info("backend started", "name", *name, "addr", srv.Addr, "cores", *cores, "db_pool", *dbPool)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("error starting server: %v", err)
	}
}
