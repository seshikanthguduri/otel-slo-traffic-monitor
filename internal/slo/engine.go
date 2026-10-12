package slo

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

// WindowResult is the SLI over one PromQL window.
type WindowResult struct {
	Window     string  `json:"window"`
	HasData    bool    `json:"has_data"`
	ErrorRatio float64 `json:"error_ratio"`
	BurnRate   float64 `json:"burn_rate"`
}

// AlertState is the evaluation of one multi-window burn-rate policy.
type AlertState struct {
	Severity    string  `json:"severity"`
	LongWindow  string  `json:"long_window"`
	ShortWindow string  `json:"short_window"`
	Threshold   float64 `json:"threshold"`
	LongBurn    float64 `json:"long_burn"`
	ShortBurn   float64 `json:"short_burn"`
	Firing      bool    `json:"firing"`
}

// Status is the full evaluation of one SLO (and its SLA, if any).
type Status struct {
	Name                   string         `json:"name"`
	Service                string         `json:"service"`
	Description            string         `json:"description,omitempty"`
	Objective              float64        `json:"objective"`
	Window                 string         `json:"window"`
	HasData                bool           `json:"has_data"`
	SLI                    float64        `json:"sli"`
	ErrorBudgetRemaining   float64        `json:"error_budget_remaining"`
	ErrorBudgetMinutesLeft float64        `json:"error_budget_minutes_left"`
	Windows                []WindowResult `json:"windows"`
	Alerts                 []AlertState   `json:"alerts"`
	SLA                    *SLAStatus     `json:"sla,omitempty"`
	EvaluatedAt            time.Time      `json:"evaluated_at"`
	Error                  string         `json:"error,omitempty"`
}

// Firing reports whether any burn-rate alert of the given severity fires.
func (s Status) Firing(severity string) bool {
	for _, a := range s.Alerts {
		if a.Firing && a.Severity == severity {
			return true
		}
	}
	return false
}

// Engine periodically evaluates every SLO.
type Engine struct {
	cfg *Config
	q   prom.Querier
	log *slog.Logger
	now func() time.Time

	mu       sync.RWMutex
	statuses []Status
	ready    atomic.Bool
}

// NewEngine builds an engine. log may be nil.
func NewEngine(cfg *Config, q prom.Querier, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{cfg: cfg, q: q, log: log, now: time.Now}
}

// Ready is true after the first evaluation cycle.
func (e *Engine) Ready() bool { return e.ready.Load() }

// Snapshot returns the latest statuses.
func (e *Engine) Snapshot() []Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Status, len(e.statuses))
	copy(out, e.statuses)
	return out
}

// Run evaluates on every interval until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(time.Duration(e.cfg.EvaluationInterval))
	defer t.Stop()
	for {
		e.EvaluateOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EvaluateOnce evaluates all SLOs concurrently and stores the result.
func (e *Engine) EvaluateOnce(ctx context.Context) []Status {
	out := make([]Status, len(e.cfg.SLOs))
	var wg sync.WaitGroup
	for i, s := range e.cfg.SLOs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = e.evaluate(ctx, s)
		}()
	}
	wg.Wait()
	e.mu.Lock()
	e.statuses = out
	e.mu.Unlock()
	e.ready.Store(true)
	for _, s := range out {
		lvl := slog.LevelDebug
		if s.Firing("page") || s.Error != "" {
			lvl = slog.LevelWarn
		}
		e.log.Log(ctx, lvl, "slo evaluated", "slo", s.Name, "sli", s.SLI, "budget_remaining", s.ErrorBudgetRemaining,
			"page", s.Firing("page"), "ticket", s.Firing("ticket"), "error", s.Error)
	}
	return out
}

func (e *Engine) evaluate(ctx context.Context, s SLO) Status {
	st := Status{Name: s.Name, Service: s.Service, Description: s.Description,
		Objective: s.Objective, Window: s.Window, EvaluatedAt: e.now().UTC()}
	badT, totalT, err := s.Queries()
	if err != nil {
		st.Error = err.Error()
		return st
	}
	byWindow := map[string]WindowResult{}
	var errs []error
	for _, w := range e.cfg.Windows(s) {
		r, err := e.window(ctx, badT, totalT, w, s.Objective)
		if err != nil {
			errs = append(errs, err)
		}
		byWindow[w] = r
		st.Windows = append(st.Windows, r)
	}
	if err := errors.Join(errs...); err != nil {
		st.Error = err.Error()
	}

	main := byWindow[s.Window]
	st.HasData = main.HasData
	if main.HasData {
		st.SLI = 1 - main.ErrorRatio
		st.ErrorBudgetRemaining = BudgetRemaining(main.ErrorRatio, s.Objective)
		wd, _ := ParseWindow(s.Window)
		st.ErrorBudgetMinutesLeft = st.ErrorBudgetRemaining * ErrorBudget(s.Objective) * wd.Minutes()
	}
	for _, p := range e.cfg.AlertPolicies {
		th := BurnRateThreshold(p, s.Window)
		l, sh := byWindow[p.LongWindow], byWindow[p.ShortWindow]
		st.Alerts = append(st.Alerts, AlertState{
			Severity: p.Severity, LongWindow: p.LongWindow, ShortWindow: p.ShortWindow,
			Threshold: th, LongBurn: l.BurnRate, ShortBurn: sh.BurnRate,
			// Both windows must exceed the threshold: the long window gives
			// significance, the short window makes the alert reset quickly.
			Firing: l.HasData && sh.HasData && l.BurnRate > th && sh.BurnRate > th,
		})
	}
	if s.SLA != nil {
		sla := EvaluateSLA(*s.SLA, st.SLI, s.Objective, st.HasData)
		st.SLA = &sla
	}
	return st
}

func (e *Engine) window(ctx context.Context, badT, totalT, w string, objective float64) (WindowResult, error) {
	r := WindowResult{Window: w}
	total, err := prom.Scalar(ctx, e.q, Render(totalT, w))
	if errors.Is(err, prom.ErrNoData) || (err == nil && total == 0) {
		return r, nil // no traffic is not an error and does not burn budget
	}
	if err != nil {
		return r, err
	}
	bad, err := prom.Scalar(ctx, e.q, Render(badT, w))
	if errors.Is(err, prom.ErrNoData) {
		bad, err = 0, nil
	}
	if err != nil {
		return r, err
	}
	r.HasData = true
	r.ErrorRatio = clamp01(bad / total)
	r.BurnRate = BurnRate(r.ErrorRatio, objective)
	return r, nil
}

// RegisterMetrics exposes the latest evaluation as OpenTelemetry gauges so
// SLO state flows through the same Collector pipeline as everything else.
func (e *Engine) RegisterMetrics(m metric.Meter) error {
	sli, err1 := m.Float64ObservableGauge("slo.sli", metric.WithDescription("SLI over the SLO window (good/total)"))
	budget, err2 := m.Float64ObservableGauge("slo.error_budget.remaining", metric.WithDescription("Fraction of error budget left; negative when overspent"))
	objective, err3 := m.Float64ObservableGauge("slo.objective", metric.WithDescription("SLO target"))
	burn, err4 := m.Float64ObservableGauge("slo.burn_rate", metric.WithDescription("Error-budget burn rate per window"))
	alert, err5 := m.Int64ObservableGauge("slo.alert.firing", metric.WithDescription("1 when a multi-window burn-rate policy fires"))
	slaMet, err6 := m.Int64ObservableGauge("sla.compliant", metric.WithDescription("1 when the SLA target is met"))
	slaCredit, err7 := m.Float64ObservableGauge("sla.credit.percent", metric.WithDescription("Service credit owed under the SLA tiers"))
	if err := errors.Join(err1, err2, err3, err4, err5, err6, err7); err != nil {
		return err
	}
	_, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, s := range e.Snapshot() {
			base := []attribute.KeyValue{attribute.String("slo", s.Name), attribute.String("slo.service", s.Service)}
			o.ObserveFloat64(objective, s.Objective, metric.WithAttributes(base...))
			if !s.HasData {
				continue
			}
			o.ObserveFloat64(sli, s.SLI, metric.WithAttributes(base...))
			o.ObserveFloat64(budget, s.ErrorBudgetRemaining, metric.WithAttributes(base...))
			for _, w := range s.Windows {
				if w.HasData {
					o.ObserveFloat64(burn, w.BurnRate, metric.WithAttributes(append(base, attribute.String("window", w.Window))...))
				}
			}
			for _, a := range s.Alerts {
				v := int64(0)
				if a.Firing {
					v = 1
				}
				o.ObserveInt64(alert, v, metric.WithAttributes(append(base,
					attribute.String("severity", a.Severity), attribute.String("long_window", a.LongWindow))...))
			}
			if s.SLA != nil && s.SLA.State != SLANoData {
				v := int64(1)
				if s.SLA.State == SLABreached {
					v = 0
				}
				o.ObserveInt64(slaMet, v, metric.WithAttributes(base...))
				o.ObserveFloat64(slaCredit, s.SLA.CreditPercent, metric.WithAttributes(base...))
			}
		}
		return nil
	}, sli, budget, objective, burn, alert, slaMet, slaCredit)
	return err
}
