// Package containers joins container resource metrics (kubeletstats and
// k8s_cluster receivers) with per-pod RED metrics (OpenTelemetry HTTP
// semantic conventions) into one container-level health view. This answers
// "is the SLO burn caused by the code, the dependency, or the container
// running out of CPU/memory?" without leaving one API call.
package containers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

// Queries are PromQL expressions. {{window}} and {{selector}} are replaced.
// Every query must aggregate "by (k8s_namespace_name, k8s_pod_name, k8s_container_name)"
// except the RED queries, which aggregate by pod (one app container per pod).
type Queries struct {
	Selector         string  `yaml:"selector" json:"selector"`
	CPUCores         string  `yaml:"cpuCores" json:"cpu_cores"`
	CPULimitUtil     string  `yaml:"cpuLimitUtilization" json:"cpu_limit_utilization"`
	MemoryBytes      string  `yaml:"memoryBytes" json:"memory_bytes"`
	MemoryLimitUtil  string  `yaml:"memoryLimitUtilization" json:"memory_limit_utilization"`
	Restarts         string  `yaml:"restarts" json:"restarts"`
	RequestRate      string  `yaml:"requestRate" json:"request_rate"`
	ErrorRate        string  `yaml:"errorRate" json:"error_rate"`
	P99              string  `yaml:"p99" json:"p99"`
	CPUSaturation    float64 `yaml:"cpuSaturationThreshold" json:"cpu_saturation_threshold"`
	MemorySaturation float64 `yaml:"memorySaturationThreshold" json:"memory_saturation_threshold"`
}

const by = "by (k8s_namespace_name, k8s_pod_name, k8s_container_name)"

// DefaultQueries match the metric names produced by
// otelcol-contrib kubeletstats + k8s_cluster receivers and the Collector's
// Prometheus exporter (with unit suffixes and resource_to_telemetry_conversion).
func DefaultQueries() Queries {
	return Queries{
		CPUCores:         "sum " + by + " (container_cpu_usage{{selector}})",
		CPULimitUtil:     "max " + by + " (k8s_container_cpu_limit_utilization_ratio{{selector}})",
		MemoryBytes:      "sum " + by + " (container_memory_working_set_bytes{{selector}})",
		MemoryLimitUtil:  "max " + by + " (k8s_container_memory_limit_utilization_ratio{{selector}})",
		Restarts:         "max " + by + " (k8s_container_restarts{{selector}})",
		RequestRate:      "sum by (k8s_namespace_name, k8s_pod_name) (rate(http_server_request_duration_seconds_count{{selector}}[{{window}}]))",
		ErrorRate:        `sum by (k8s_namespace_name, k8s_pod_name) (rate(http_server_request_duration_seconds_count{{selector5xx}}[{{window}}]))`,
		P99:              "histogram_quantile(0.99, sum by (k8s_namespace_name, k8s_pod_name, le) (rate(http_server_request_duration_seconds_bucket{{selector}}[{{window}}])))",
		CPUSaturation:    0.8,
		MemorySaturation: 0.9,
	}
}

// WithDefaults fills empty fields from DefaultQueries.
func (q Queries) WithDefaults() Queries {
	d := DefaultQueries()
	fill := func(dst *string, def string) {
		if *dst == "" {
			*dst = def
		}
	}
	fill(&q.CPUCores, d.CPUCores)
	fill(&q.CPULimitUtil, d.CPULimitUtil)
	fill(&q.MemoryBytes, d.MemoryBytes)
	fill(&q.MemoryLimitUtil, d.MemoryLimitUtil)
	fill(&q.Restarts, d.Restarts)
	fill(&q.RequestRate, d.RequestRate)
	fill(&q.ErrorRate, d.ErrorRate)
	fill(&q.P99, d.P99)
	if q.CPUSaturation == 0 {
		q.CPUSaturation = d.CPUSaturation
	}
	if q.MemorySaturation == 0 {
		q.MemorySaturation = d.MemorySaturation
	}
	return q
}

// Stat is the merged view of one container.
type Stat struct {
	Namespace       string   `json:"namespace"`
	Pod             string   `json:"pod"`
	Container       string   `json:"container"`
	CPUCores        float64  `json:"cpu_cores"`
	CPULimitUtil    *float64 `json:"cpu_limit_utilization,omitempty"`
	MemoryBytes     float64  `json:"memory_bytes"`
	MemoryLimitUtil *float64 `json:"memory_limit_utilization,omitempty"`
	Restarts        float64  `json:"restarts"`
	RequestRate     float64  `json:"request_rate"`
	ErrorRatio      float64  `json:"error_ratio"`
	P99Seconds      float64  `json:"p99_seconds"`
	Saturated       bool     `json:"saturated"`
	Reasons         []string `json:"reasons,omitempty"`
}

// Collector builds container stats from Prometheus.
type Collector struct {
	Q       prom.Querier
	Queries Queries
}

func (c Collector) render(tmpl, window string) string {
	sel := ""
	sel5 := `{http_response_status_code=~"5.."}`
	if s := c.Queries.Selector; s != "" {
		sel = "{" + s + "}"
		sel5 = `{` + s + `,http_response_status_code=~"5.."}`
	}
	r := strings.NewReplacer("{{selector5xx}}", sel5, "{{selector}}", sel, "{{window}}", window)
	return r.Replace(tmpl)
}

type ck struct{ ns, pod, ctr string }

// Collect runs all queries. Missing metric families are tolerated (they show
// as zero/nil) so the view degrades gracefully on clusters without limits.
func (c Collector) Collect(ctx context.Context, window string) ([]Stat, error) {
	q := c.Queries.WithDefaults()
	c.Queries = q
	run := func(t string) ([]prom.Sample, error) { return c.Q.Query(ctx, c.render(t, window)) }

	stats := map[ck]*Stat{}
	get := func(l map[string]string) *Stat {
		k := ck{l["k8s_namespace_name"], l["k8s_pod_name"], l["k8s_container_name"]}
		if k.pod == "" {
			return nil
		}
		if stats[k] == nil {
			stats[k] = &Stat{Namespace: k.ns, Pod: k.pod, Container: k.ctr}
		}
		return stats[k]
	}
	type setter func(*Stat, float64)
	perContainer := []struct {
		q   string
		set setter
	}{
		{q.CPUCores, func(s *Stat, v float64) { s.CPUCores = v }},
		{q.CPULimitUtil, func(s *Stat, v float64) { s.CPULimitUtil = &v }},
		{q.MemoryBytes, func(s *Stat, v float64) { s.MemoryBytes = v }},
		{q.MemoryLimitUtil, func(s *Stat, v float64) { s.MemoryLimitUtil = &v }},
		{q.Restarts, func(s *Stat, v float64) { s.Restarts = v }},
	}
	for _, pc := range perContainer {
		res, err := run(pc.q)
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", pc.q, err)
		}
		for _, smp := range res {
			if st := get(smp.Labels); st != nil && finite(smp.Value) {
				pc.set(st, smp.Value)
			}
		}
	}

	// RED metrics are per pod; attach them to every container of that pod
	// that reports resource metrics (or create a row if none do).
	type pk struct{ ns, pod string }
	red := map[pk]*[3]float64{}
	for i, t := range []string{q.RequestRate, q.ErrorRate, q.P99} {
		res, err := run(t)
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", t, err)
		}
		for _, smp := range res {
			k := pk{smp.Labels["k8s_namespace_name"], smp.Labels["k8s_pod_name"]}
			if k.pod == "" || !finite(smp.Value) {
				continue
			}
			if red[k] == nil {
				red[k] = &[3]float64{}
			}
			red[k][i] = smp.Value
		}
	}
	for k, v := range red {
		matched := false
		for key, st := range stats {
			if key.ns == k.ns && key.pod == k.pod {
				apply(st, v)
				matched = true
			}
		}
		if !matched {
			st := &Stat{Namespace: k.ns, Pod: k.pod}
			apply(st, v)
			stats[ck{k.ns, k.pod, ""}] = st
		}
	}

	out := make([]Stat, 0, len(stats))
	for _, st := range stats {
		if st.CPULimitUtil != nil && *st.CPULimitUtil > q.CPUSaturation {
			st.Reasons = append(st.Reasons, fmt.Sprintf("cpu at %.0f%% of limit", 100**st.CPULimitUtil))
		}
		if st.MemoryLimitUtil != nil && *st.MemoryLimitUtil > q.MemorySaturation {
			st.Reasons = append(st.Reasons, fmt.Sprintf("memory at %.0f%% of limit", 100**st.MemoryLimitUtil))
		}
		if st.Restarts > 0 {
			st.Reasons = append(st.Reasons, fmt.Sprintf("%.0f restarts", st.Restarts))
		}
		st.Saturated = len(st.Reasons) > 0
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		return a.Container < b.Container
	})
	return out, nil
}

func apply(st *Stat, v *[3]float64) {
	st.RequestRate, st.P99Seconds = v[0], v[2]
	if v[0] > 0 {
		st.ErrorRatio = math.Min(1, v[1]/v[0])
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
