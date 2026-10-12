package slo

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

const testCfg = `
prometheus:
  url: http://prom:9090
slos:
  - name: checkout-availability
    service: checkout
    objective: 0.999
    window: 30d
    sli:
      template: http-availability
      selector: k8s_namespace_name="demo"
    sla:
      target: 0.995
      credits:
        - {below: 0.995, percent: 10}
        - {below: 0.99, percent: 25}
  - name: checkout-latency
    service: checkout
    objective: 0.99
    sli:
      template: http-latency
      latencyThreshold: "0.25"
  - name: frontend-to-checkout
    objective: 0.995
    window: 7d
    sli:
      template: servicegraph-edge-availability
      client: frontend
      server: checkout
`

func mustCfg(t *testing.T, s string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDefaultBurnRateThresholdsMatchSREWorkbook(t *testing.T) {
	want := []float64{14.4, 6, 3, 1}
	for i, p := range DefaultAlertPolicies() {
		if got := BurnRateThreshold(p, "30d"); !near(got, want[i]) {
			t.Errorf("policy %d: got %v want %v", i, got, want[i])
		}
	}
	// A 7d window scales thresholds down proportionally (2% of 7d in 1h).
	if got := BurnRateThreshold(DefaultAlertPolicies()[0], "7d"); !near(got, 0.02*168) {
		t.Errorf("7d page threshold %v", got)
	}
}

func TestMath(t *testing.T) {
	if !near(BurnRate(0.001, 0.999), 1) || !near(BudgetRemaining(0.0005, 0.999), 0.5) {
		t.Fatal("burn/budget math wrong")
	}
	if BudgetRemaining(0.002, 0.999) >= 0 {
		t.Fatal("overspent budget must be negative")
	}
}

func TestSLAStates(t *testing.T) {
	sla := SLA{Target: 0.995, Credits: []CreditTier{{Below: 0.995, Percent: 10}, {Below: 0.99, Percent: 25}}}
	cases := []struct {
		sli    float64
		state  SLAState
		credit float64
	}{
		{0.9995, SLAMet, 0},
		{0.998, SLAAtRisk, 0}, // SLO 0.999 breached, SLA fine
		{0.993, SLABreached, 10},
		{0.98, SLABreached, 25},
	}
	for _, c := range cases {
		got := EvaluateSLA(sla, c.sli, 0.999, true)
		if got.State != c.state || got.CreditPercent != c.credit {
			t.Errorf("sli %v: got %+v", c.sli, got)
		}
	}
	if EvaluateSLA(sla, 0, 0.999, false).State != SLANoData {
		t.Fatal("no data")
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		`slos: [{name: Bad_Name, service: x, objective: 0.99, sli: {template: http-availability}}]`,
		`slos: [{name: a, service: x, objective: 1.2, sli: {template: http-availability}}]`,
		`slos: [{name: a, service: x, objective: 0.99, window: 30days, sli: {template: http-availability}}]`,
		`slos: [{name: a, service: x, objective: 0.99, sli: {template: http-latency}}]`,
		`slos: [{name: a, objective: 0.99, sli: {template: custom, errorQuery: "x", totalQuery: "y"}}]`,
		`slos: [{name: a, service: x, objective: 0.99, sli: {template: http-availability}, sla: {target: 0.999}}]`,
		`unknownField: 1`,
		`alertPolicies: [{severity: page, longWindow: 5m, shortWindow: 1h, burnRate: 2}]`,
	}
	for _, b := range bad {
		if _, err := ParseConfig([]byte(b)); err == nil {
			t.Errorf("expected validation error for %s", b)
		}
	}
	c := mustCfg(t, testCfg)
	if time := c.EvaluationInterval; time == 0 || c.Traffic.Window != "5m" {
		t.Fatal("defaults not applied")
	}
	w := c.Windows(c.SLOs[0])
	if w[0] != "5m" || w[len(w)-1] != "30d" {
		t.Fatalf("windows not sorted: %v", w)
	}
}

func TestQueryTemplates(t *testing.T) {
	c := mustCfg(t, testCfg)
	bad, total, _ := c.SLOs[0].Queries()
	if !strings.Contains(bad, `http_response_status_code=~"5.."`) || !strings.Contains(total, `k8s_namespace_name="demo"`) {
		t.Fatalf("availability template: %s | %s", bad, total)
	}
	bad, _, _ = c.SLOs[1].Queries()
	if !strings.Contains(bad, `le="0.25"`) {
		t.Fatalf("latency template: %s", bad)
	}
	bad, _, _ = c.SLOs[2].Queries()
	if !strings.Contains(bad, `traces_service_graph_request_failed_total{client="frontend",server="checkout"}`) {
		t.Fatalf("edge template: %s", bad)
	}
}

// fakeQuerier returns a constant error ratio, simulating a steady incident.
type fakeQuerier struct{ errRatio float64 }

func (f fakeQuerier) Query(_ context.Context, q string) ([]prom.Sample, error) {
	v := 100.0
	if strings.Contains(q, "5..") || strings.Contains(q, "failed") || strings.Contains(q, " - ") {
		v = 100 * f.errRatio
	}
	return []prom.Sample{{Value: v}}, nil
}

type emptyQuerier struct{}

func (emptyQuerier) Query(context.Context, string) ([]prom.Sample, error) { return nil, nil }

func TestEngineFastBurnPagesAndBreachesSLA(t *testing.T) {
	c := mustCfg(t, testCfg)
	e := NewEngine(c, fakeQuerier{errRatio: 0.02}, nil) // 20x burn on 99.9%
	st := e.EvaluateOnce(context.Background())[0]
	if !st.HasData || !near(st.SLI, 0.98) {
		t.Fatalf("sli %+v", st)
	}
	if !st.Firing("page") || !st.Firing("ticket") {
		t.Fatalf("expected page+ticket: %+v", st.Alerts)
	}
	if st.SLA == nil || st.SLA.State != SLABreached || st.SLA.CreditPercent != 25 {
		t.Fatalf("sla %+v", st.SLA)
	}
	if !e.Ready() || len(e.Snapshot()) != 3 {
		t.Fatal("snapshot")
	}
}

func TestEngineSlowBurnTicketsOnly(t *testing.T) {
	c := mustCfg(t, testCfg)
	st := NewEngine(c, fakeQuerier{errRatio: 0.004}, nil).EvaluateOnce(context.Background())[0] // 4x burn
	if st.Firing("page") || !st.Firing("ticket") {
		t.Fatalf("expected ticket only: %+v", st.Alerts)
	}
	if st.ErrorBudgetRemaining >= 0 {
		t.Fatal("4x burn over full window must overspend budget")
	}
}

func TestEngineNoTrafficIsNotAnError(t *testing.T) {
	c := mustCfg(t, testCfg)
	st := NewEngine(c, emptyQuerier{}, nil).EvaluateOnce(context.Background())[0]
	if st.HasData || st.Error != "" || st.Firing("page") || st.SLA.State != SLANoData {
		t.Fatalf("unexpected %+v", st)
	}
}

func TestRenderRules(t *testing.T) {
	c := mustCfg(t, testCfg)
	b, err := RenderRules(c, "prometheus", "")
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"slo:sli_error:ratio_rate5m", "SLOErrorBudgetBurn", "SLABreached", "(14.4 * 0.001"} {
		if !strings.Contains(out, want) {
			t.Errorf("rules missing %q", want)
		}
	}
	b, _ = RenderRules(c, "prometheusrule", "observability")
	if !strings.Contains(string(b), "kind: PrometheusRule") {
		t.Fatal("CR format")
	}
	if _, err := RenderRules(c, "nope", ""); err == nil {
		t.Fatal("expected format error")
	}
}
