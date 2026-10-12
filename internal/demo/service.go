// Package demo implements a configurable microservice used to generate
// realistic east-west traffic. One binary plays every role in the demo
// topology (frontend, checkout, payment, ...) based on environment variables.
//
// Fault injection (error rate, latency, slow-request tail) can be changed at
// runtime through POST /admin/fault, which is how the burn-rate alerts are
// exercised during a demo.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Fault describes injected failure behaviour.
type Fault struct {
	ErrorRate     float64 `json:"error_rate"`      // 0..1 probability of HTTP 500
	LatencyMS     int     `json:"latency_ms"`      // base latency added to every request
	JitterMS      int     `json:"jitter_ms"`       // uniform random jitter on top
	SlowRate      float64 `json:"slow_rate"`       // 0..1 probability of a slow request
	SlowLatencyMS int     `json:"slow_latency_ms"` // latency of a slow request
}

// Validate rejects impossible fault settings.
func (f Fault) Validate() error {
	switch {
	case f.ErrorRate < 0 || f.ErrorRate > 1:
		return errors.New("error_rate must be within [0,1]")
	case f.SlowRate < 0 || f.SlowRate > 1:
		return errors.New("slow_rate must be within [0,1]")
	case f.LatencyMS < 0 || f.JitterMS < 0 || f.SlowLatencyMS < 0:
		return errors.New("latencies must be >= 0")
	}
	return nil
}

// Config is the static service configuration.
type Config struct {
	Name        string
	Downstreams []string // full URLs called on every request, in order
	Fault       Fault
	Timeout     time.Duration
}

// Service is an HTTP microservice instrumented with OpenTelemetry.
type Service struct {
	cfg    Config
	client *http.Client
	tracer trace.Tracer

	mu    sync.RWMutex
	fault Fault
	rnd   func() float64

	processed metric.Int64Counter
}

// New builds a Service. rnd may be nil (defaults to math/rand).
func New(cfg Config, rnd func() float64) (*Service, error) {
	if cfg.Name == "" {
		return nil, errors.New("service name is required")
	}
	if err := cfg.Fault.Validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	meter := otel.Meter("github.com/seshikanthguduri/otel-slo-traffic-monitor/demo")
	processed, err := meter.Int64Counter("app.requests.processed",
		metric.WithDescription("Business requests processed, by outcome"),
		metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg: cfg,
		// otelhttp.NewTransport creates CLIENT spans and injects W3C traceparent,
		// which is what lets the Collector's servicegraph connector draw edges.
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		tracer:    otel.Tracer("github.com/seshikanthguduri/otel-slo-traffic-monitor/demo"),
		fault:     cfg.Fault,
		rnd:       rnd,
		processed: processed,
	}, nil
}

// Handler returns the instrumented HTTP handler.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		// One otelhttp handler per route so spans and metrics carry a low
		// cardinality operation name instead of raw URLs.
		mux.Handle(pattern, otelhttp.NewHandler(h, pattern))
	}
	route("GET /api/work", s.handleWork)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /admin/fault", s.handleGetFault)
	mux.HandleFunc("POST /admin/fault", s.handleSetFault)
	return mux
}

// CurrentFault returns the active fault settings.
func (s *Service) CurrentFault() Fault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fault
}

// SetFault replaces the active fault settings.
func (s *Service) SetFault(f Fault) error {
	if err := f.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.fault = f
	s.mu.Unlock()
	return nil
}

type workResponse struct {
	Service     string             `json:"service"`
	Status      int                `json:"status"`
	TraceID     string             `json:"trace_id,omitempty"`
	Downstreams []downstreamResult `json:"downstreams,omitempty"`
	Error       string             `json:"error,omitempty"`
}

type downstreamResult struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Service) handleWork(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	f := s.CurrentFault()

	resp := workResponse{Service: s.cfg.Name, Status: http.StatusOK}
	if sc := span.SpanContext(); sc.HasTraceID() {
		resp.TraceID = sc.TraceID().String()
	}

	if err := sleepCtx(ctx, s.latency(f)); err != nil {
		resp.Status, resp.Error = http.StatusGatewayTimeout, "request cancelled"
		s.finish(ctx, w, resp)
		return
	}

	for _, url := range s.cfg.Downstreams {
		dr := s.callDownstream(ctx, url)
		resp.Downstreams = append(resp.Downstreams, dr)
		if dr.Error != "" || dr.Status >= 500 {
			resp.Status, resp.Error = http.StatusBadGateway, "dependency failed: "+url
			span.SetStatus(codes.Error, resp.Error)
			s.finish(ctx, w, resp)
			return
		}
	}

	if s.rnd() < f.ErrorRate {
		resp.Status, resp.Error = http.StatusInternalServerError, "injected fault"
		span.SetStatus(codes.Error, resp.Error)
	}
	s.finish(ctx, w, resp)
}

func (s *Service) finish(ctx context.Context, w http.ResponseWriter, resp workResponse) {
	outcome := "success"
	if resp.Status >= 500 {
		outcome = "failure"
	}
	s.processed.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Service) callDownstream(ctx context.Context, url string) downstreamResult {
	ctx, span := s.tracer.Start(ctx, "call "+hostOf(url), trace.WithAttributes(attribute.String("downstream.url", url)))
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return downstreamResult{URL: url, Error: err.Error()}
	}
	res, err := s.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return downstreamResult{URL: url, Error: err.Error()}
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 500 {
		span.SetStatus(codes.Error, res.Status)
	}
	return downstreamResult{URL: url, Status: res.StatusCode}
}

func (s *Service) latency(f Fault) time.Duration {
	ms := float64(f.LatencyMS)
	if f.JitterMS > 0 {
		ms += s.rnd() * float64(f.JitterMS)
	}
	if f.SlowRate > 0 && s.rnd() < f.SlowRate {
		ms = float64(f.SlowLatencyMS)
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func (s *Service) handleGetFault(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.CurrentFault())
}

// handleSetFault accepts either a JSON body or query parameters, e.g.
// curl -XPOST 'localhost:8080/admin/fault?error_rate=0.2&latency_ms=50'
func (s *Service) handleSetFault(w http.ResponseWriter, r *http.Request) {
	f := s.CurrentFault()
	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if err := applyQuery(&f, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.SetFault(f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleGetFault(w, r)
}

func applyQuery(f *Fault, r *http.Request) error {
	q := r.URL.Query()
	floats := map[string]*float64{"error_rate": &f.ErrorRate, "slow_rate": &f.SlowRate}
	ints := map[string]*int{"latency_ms": &f.LatencyMS, "jitter_ms": &f.JitterMS, "slow_latency_ms": &f.SlowLatencyMS}
	for k, p := range floats {
		if v := q.Get(k); v != "" {
			x, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*p = x
		}
	}
	for k, p := range ints {
		if v := q.Get(k); v != "" {
			x, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*p = x
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func hostOf(url string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(url, "http://"), "https://")
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		return u[:i]
	}
	return u
}

// ParseDownstreams splits a comma separated URL list, ignoring blanks.
func ParseDownstreams(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
