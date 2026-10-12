package traffic

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

func s(c, sv string, v float64) prom.Sample {
	return prom.Sample{Labels: map[string]string{"client": c, "server": sv}, Value: v}
}

func sampleGraph() *Graph {
	rps := []prom.Sample{
		s("loadgen", "frontend", 10), s("frontend", "checkout", 6), s("frontend", "catalog", 10),
		s("checkout", "payment", 6), s("checkout", "inventory", 6),
	}
	errs := []prom.Sample{s("checkout", "payment", 0.6)}
	p50 := []prom.Sample{s("checkout", "payment", 0.02)}
	p99 := []prom.Sample{s("checkout", "payment", 0.12), s("frontend", "catalog", 0.9), s("frontend", "checkout", math.NaN())}
	return Analyze("5m", rps, errs, p50, p99, Thresholds{ErrorRatio: 0.01, P99Seconds: 0.5})
}

func TestAnalyzeHotEdgesAndBlastRadius(t *testing.T) {
	g := sampleGraph()
	if len(g.Edges) != 5 || len(g.Nodes) != 6 {
		t.Fatalf("edges=%d nodes=%d", len(g.Edges), len(g.Nodes))
	}
	hot := g.HotEdges()
	if len(hot) != 2 {
		t.Fatalf("want 2 hot edges, got %+v", hot)
	}
	var payment, loadgen Node
	for _, n := range g.Nodes {
		switch n.Name {
		case "payment":
			payment = n
		case "loadgen":
			loadgen = n
		}
	}
	if strings.Join(payment.BlastRadius, ",") != "checkout,frontend,loadgen" {
		t.Fatalf("blast radius %v", payment.BlastRadius)
	}
	if !loadgen.Entrypoint || payment.Entrypoint {
		t.Fatal("entrypoint detection")
	}
	for _, e := range g.Edges {
		if e.Client == "checkout" && e.Server == "payment" && math.Abs(e.ErrorRatio-0.1) > 1e-9 {
			t.Fatalf("error ratio %v", e.ErrorRatio)
		}
	}
}

func TestCycleSafe(t *testing.T) {
	g := Analyze("5m", []prom.Sample{s("a", "b", 1), s("b", "a", 1)}, nil, nil, nil, Thresholds{})
	for _, n := range g.Nodes {
		if len(n.BlastRadius) != 1 {
			t.Fatalf("cycle handling: %+v", n)
		}
	}
}

func TestRenderers(t *testing.T) {
	g := sampleGraph()
	m := g.Mermaid()
	if !strings.HasPrefix(m, "flowchart LR") || !strings.Contains(m, "linkStyle") {
		t.Fatalf("mermaid:\n%s", m)
	}
	if !strings.Contains(g.DOT(), `"checkout" -> "payment"`) {
		t.Fatal("dot")
	}
}

type recQ struct{ queries []string }

func (r *recQ) Query(_ context.Context, q string) ([]prom.Sample, error) {
	r.queries = append(r.queries, q)
	return []prom.Sample{s("a", "b", 1)}, nil
}

func TestBuildQueries(t *testing.T) {
	r := &recQ{}
	g, err := Analyzer{Q: r, Selector: `k8s_cluster_name="dev"`}.Build(context.Background(), "5m")
	if err != nil || len(g.Edges) != 1 || len(r.queries) != 4 {
		t.Fatalf("g=%+v err=%v q=%d", g, err, len(r.queries))
	}
	if !strings.Contains(r.queries[0], `{k8s_cluster_name="dev"}[5m]`) {
		t.Fatalf("selector not applied: %s", r.queries[0])
	}
}
