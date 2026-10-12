package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/containers"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/slo"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/traffic"
)

type q struct{}

func (q) Query(_ context.Context, s string) ([]prom.Sample, error) {
	if strings.Contains(s, "traces_service_graph") {
		v := 10.0
		if strings.Contains(s, "failed") {
			v = 5
		}
		return []prom.Sample{{Labels: map[string]string{"client": "checkout", "server": "payment"}, Value: v}}, nil
	}
	v := 100.0
	if strings.Contains(s, "5..") {
		v = 3
	}
	return []prom.Sample{{Labels: map[string]string{}, Value: v}}, nil
}

func newServer(t *testing.T) *Server {
	t.Helper()
	cfg, err := slo.ParseConfig([]byte(`
slos:
  - name: payment-availability
    service: payment
    objective: 0.999
    sli: {template: http-availability}
    sla: {target: 0.99, credits: [{below: 0.99, percent: 10}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := slo.NewEngine(cfg, q{}, nil)
	return &Server{Engine: e, TrafficWindow: "5m",
		Traffic:    traffic.Analyzer{Q: q{}, T: traffic.Thresholds{ErrorRatio: 0.01}},
		Containers: containers.Collector{Q: q{}}}
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestReadinessAndEndpoints(t *testing.T) {
	s := newServer(t)
	h := s.Handler()
	if do(t, h, "/readyz").Code != http.StatusServiceUnavailable {
		t.Fatal("must not be ready before first evaluation")
	}
	s.Engine.EvaluateOnce(context.Background())
	if do(t, h, "/readyz").Code != http.StatusOK {
		t.Fatal("ready")
	}
	for _, p := range []string{"/api/v1/slos", "/api/v1/slos/payment-availability", "/api/v1/sla",
		"/api/v1/traffic", "/api/v1/traffic/mermaid", "/api/v1/traffic/dot", "/api/v1/containers"} {
		if rec := do(t, h, p); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
		}
	}
	if do(t, h, "/api/v1/slos/nope").Code != http.StatusNotFound {
		t.Error("404 expected")
	}
}

func TestSummary(t *testing.T) {
	s := newServer(t)
	s.Engine.EvaluateOnce(context.Background())
	var sum Summary
	if err := json.Unmarshal(do(t, s.Handler(), "/api/v1/summary").Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	// 3% errors against a 99.9% objective: 30x burn, SLA (99%) breached.
	if len(sum.PagesFiring) != 1 || len(sum.SLAsBreached) != 1 || len(sum.HotEdges) != 1 {
		t.Fatalf("summary %+v", sum)
	}
}
