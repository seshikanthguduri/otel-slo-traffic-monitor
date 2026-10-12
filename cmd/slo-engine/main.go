// Command slo-engine evaluates SLOs/SLAs, analyses service-to-service traffic
// and container health from any Prometheus-compatible backend.
//
//	slo-engine serve    -config slo-config.yaml   # long-running API + OTel metrics
//	slo-engine report   -config slo-config.yaml   # one-shot table for CI / chatops
//	slo-engine rules    -config slo-config.yaml   # generate Prometheus alert rules
//	slo-engine validate -config slo-config.yaml   # lint the config
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/api"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/containers"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/env"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/slo"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/telemetry"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/traffic"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/version"
)

func usage() {
	fmt.Fprintf(os.Stderr, `slo-engine %s

Usage: slo-engine <serve|report|rules|validate> [-config path] [flags]
`, version.Version)
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", env.String("SLO_CONFIG", "slo-config.yaml"), "path to SLO config")
	listen := fs.String("listen", env.String("LISTEN_ADDR", ":8081"), "serve: listen address")
	format := fs.String("format", "prometheus", "rules: prometheus | prometheusrule")
	namespace := fs.String("namespace", "observability", "rules: namespace for PrometheusRule")
	jsonOut := fs.Bool("json", false, "report: print JSON")
	failOnPage := fs.Bool("fail-on-page", false, "report: exit 2 when any page-level burn alert fires")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	cfg, err := slo.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:\n"+err.Error())
		os.Exit(1)
	}

	switch cmd {
	case "validate":
		fmt.Printf("OK: %d SLOs, %d alert policies, prometheus=%s\n", len(cfg.SLOs), len(cfg.AlertPolicies), cfg.Prometheus.URL)
	case "rules":
		b, err := slo.RenderRules(cfg, *format, *namespace)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case "report":
		os.Exit(report(cfg, *jsonOut, *failOnPage))
	case "serve":
		if err := serve(cfg, *listen, log); err != nil {
			log.Error("exit", "error", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func logLevel() slog.Level {
	if os.Getenv("LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func newQuerier(cfg *slo.Config) prom.Querier {
	c := prom.NewClient(cfg.Prometheus.URL, nil)
	for k, v := range cfg.Prometheus.Headers {
		c.Header.Set(k, os.ExpandEnv(v)) // allow "Bearer ${PROM_TOKEN}"
	}
	return c
}

func serve(cfg *slo.Config, listen string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.Setup(ctx, "slo-engine", version.Version)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	q := newQuerier(cfg)
	engine := slo.NewEngine(cfg, q, log)
	if err := engine.RegisterMetrics(otel.Meter("github.com/seshikanthguduri/otel-slo-traffic-monitor/slo")); err != nil {
		return err
	}
	go engine.Run(ctx)

	s := &api.Server{
		Engine:        engine,
		TrafficWindow: cfg.Traffic.Window,
		Traffic: traffic.Analyzer{Q: q, T: traffic.Thresholds{
			ErrorRatio: cfg.Traffic.ErrorRatioThreshold, P99Seconds: cfg.Traffic.P99ThresholdSeconds}},
		Containers: containers.Collector{Q: q, Queries: cfg.Containers},
	}
	srv := &http.Server{Addr: listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("slo-engine listening", "addr", listen, "slos", len(cfg.SLOs), "prometheus", cfg.Prometheus.URL, "version", version.Version)
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

func report(cfg *slo.Config, asJSON, failOnPage bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := newQuerier(cfg)
	statuses := slo.NewEngine(cfg, q, slog.New(slog.DiscardHandler)).EvaluateOnce(ctx)
	g, gerr := traffic.Analyzer{Q: q, T: traffic.Thresholds{
		ErrorRatio: cfg.Traffic.ErrorRatioThreshold, P99Seconds: cfg.Traffic.P99ThresholdSeconds}}.Build(ctx, cfg.Traffic.Window)

	paging := false
	for _, s := range statuses {
		paging = paging || s.Firing("page")
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"slos": statuses, "traffic": g})
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SLO\tSERVICE\tOBJECTIVE\tSLI\tBUDGET LEFT\tPAGE\tTICKET\tSLA")
		for _, s := range statuses {
			sla := "-"
			if s.SLA != nil {
				sla = fmt.Sprintf("%s (%.2f%%)", s.SLA.State, 100*s.SLA.Target)
			}
			sli, budget := "no data", "-"
			if s.HasData {
				sli, budget = fmt.Sprintf("%.4f%%", 100*s.SLI), fmt.Sprintf("%.1f%%", 100*s.ErrorBudgetRemaining)
			}
			if s.Error != "" {
				sli = "error: " + s.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%.3f%%\t%s\t%s\t%v\t%v\t%s\n", s.Name, s.Service, 100*s.Objective, sli, budget,
				s.Firing("page"), s.Firing("ticket"), sla)
		}
		tw.Flush()
		if gerr == nil {
			fmt.Println("\nTRAFFIC (" + g.Window + ")")
			tw = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "CLIENT\tSERVER\tRPS\tERROR%\tP99(ms)\tHOT")
			for _, e := range g.Edges {
				fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%.0f\t%s\n", e.Client, e.Server, e.RPS, 100*e.ErrorRatio, 1000*e.P99Seconds, hot(e))
			}
			tw.Flush()
		} else {
			fmt.Println("\ntraffic graph unavailable:", gerr)
		}
	}
	if failOnPage && paging {
		return 2
	}
	return 0
}

func hot(e traffic.Edge) string {
	if !e.Hot {
		return ""
	}
	return fmt.Sprint(e.HotReasons)
}
