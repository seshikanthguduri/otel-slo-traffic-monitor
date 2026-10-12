// Command loadgen produces steady, traced traffic against the demo entrypoint.
// It is itself an OpenTelemetry client, so it appears as the root node of the
// service graph.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/env"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/telemetry"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/version"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, env.String("OTEL_SERVICE_NAME", "loadgen"), version.Version)
	if err != nil {
		log.Error("telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	target := env.String("TARGET_URL", "http://frontend:8080/api/work")
	rps := env.Float("RPS", 10)
	workers := env.Int("CONCURRENCY", 8)
	if d := env.Duration("DURATION", 0); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	tracer := otel.Tracer("loadgen")
	jobs := make(chan struct{}, workers)
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				rctx, span := tracer.Start(ctx, "loadgen request")
				req, _ := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
				res, err := client.Do(req)
				if err != nil || res.StatusCode >= 500 {
					failed.Add(1)
				} else {
					ok.Add(1)
				}
				if res != nil {
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
				}
				span.End()
			}
		}()
	}

	log.Info("generating load", "target", target, "rps", rps, "concurrency", workers)
	tick := time.NewTicker(time.Duration(float64(time.Second) / rps))
	report := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	defer report.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-report.C:
			log.Info("progress", "ok", ok.Load(), "failed", failed.Load())
		case <-tick.C:
			select {
			case jobs <- struct{}{}:
			default: // workers saturated; drop instead of building a backlog
			}
		}
	}
	close(jobs)
	wg.Wait()
	log.Info("done", "ok", ok.Load(), "failed", failed.Load())
}
