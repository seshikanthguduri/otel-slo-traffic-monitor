package containers

import (
	"context"
	"strings"
	"testing"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

type fakeQ struct{ seen []string }

func lbl(pod, ctr string) map[string]string {
	return map[string]string{"k8s_namespace_name": "demo", "k8s_pod_name": pod, "k8s_container_name": ctr}
}

func (f *fakeQ) Query(_ context.Context, q string) ([]prom.Sample, error) {
	f.seen = append(f.seen, q)
	switch {
	case strings.Contains(q, "cpu_limit_utilization"):
		return []prom.Sample{{Labels: lbl("payment-1", "app"), Value: 0.95}}, nil
	case strings.Contains(q, "container_cpu_usage"):
		return []prom.Sample{{Labels: lbl("payment-1", "app"), Value: 0.19}, {Labels: lbl("checkout-1", "app"), Value: 0.05}}, nil
	case strings.Contains(q, "k8s_container_restarts"):
		return []prom.Sample{{Labels: lbl("checkout-1", "app"), Value: 2}}, nil
	case strings.Contains(q, `5..`):
		return []prom.Sample{{Labels: lbl("payment-1", ""), Value: 1}}, nil
	case strings.Contains(q, "histogram_quantile"):
		return []prom.Sample{{Labels: lbl("payment-1", ""), Value: 0.3}}, nil
	case strings.Contains(q, "_count"):
		return []prom.Sample{{Labels: lbl("payment-1", ""), Value: 10}, {Labels: lbl("loadgen-1", ""), Value: 5}}, nil
	}
	return nil, nil
}

func TestCollectJoinsResourceAndRED(t *testing.T) {
	f := &fakeQ{}
	out, err := Collector{Q: f, Queries: Queries{Selector: `k8s_namespace_name="demo"`}}.Collect(context.Background(), "5m")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 rows, got %+v", out)
	}
	byPod := map[string]Stat{}
	for _, s := range out {
		byPod[s.Pod] = s
	}
	p := byPod["payment-1"]
	if !p.Saturated || p.ErrorRatio != 0.1 || p.P99Seconds != 0.3 || p.RequestRate != 10 {
		t.Fatalf("payment %+v", p)
	}
	if c := byPod["checkout-1"]; !c.Saturated || c.Restarts != 2 {
		t.Fatalf("checkout %+v", c)
	}
	if byPod["loadgen-1"].Saturated {
		t.Fatal("loadgen should be healthy")
	}
	for _, q := range f.seen {
		if strings.Contains(q, "{{") {
			t.Fatalf("unrendered template: %s", q)
		}
	}
	if !strings.Contains(f.seen[len(f.seen)-2], `{k8s_namespace_name="demo",http_response_status_code=~"5.."}`) {
		t.Fatalf("5xx selector: %s", f.seen[len(f.seen)-2])
	}
}
