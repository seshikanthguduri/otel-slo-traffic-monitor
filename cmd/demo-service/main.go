// Command demo-service runs one microservice of the demo topology.
//
//	SERVICE_NAME=checkout DOWNSTREAMS=http://payment:8080/api/work,http://inventory:8080/api/work demo-service
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/demo"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/env"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/telemetry"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	name := env.String("SERVICE_NAME", env.String("OTEL_SERVICE_NAME", "demo-service"))
	shutdown, err := telemetry.Setup(ctx, name, version.Version)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	svc, err := demo.New(demo.Config{
		Name:        name,
		Downstreams: demo.ParseDownstreams(os.Getenv("DOWNSTREAMS")),
		Timeout:     env.Duration("DOWNSTREAM_TIMEOUT", 3*time.Second),
		Fault: demo.Fault{
			ErrorRate:     env.Float("ERROR_RATE", 0),
			LatencyMS:     env.Int("LATENCY_MS", 5),
			JitterMS:      env.Int("JITTER_MS", 10),
			SlowRate:      env.Float("SLOW_RATE", 0),
			SlowLatencyMS: env.Int("SLOW_LATENCY_MS", 800),
		},
	}, nil)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              env.String("LISTEN_ADDR", ":8080"),
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "service", name, "addr", srv.Addr, "version", version.Version)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(c)
}
