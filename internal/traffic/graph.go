// Package traffic analyses east-west traffic between microservices using the
// metrics emitted by the OpenTelemetry Collector "servicegraph" connector:
//
//	traces_service_graph_request_total{client,server}
//	traces_service_graph_request_failed_total{client,server}
//	traces_service_graph_request_server_seconds_bucket{client,server,le}
//
// It builds a dependency graph, flags unhealthy edges and computes the blast
// radius of each service (every upstream that transitively depends on it).
package traffic

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/prom"
)

// Edge is a directed client -> server call path.
type Edge struct {
	Client     string   `json:"client"`
	Server     string   `json:"server"`
	RPS        float64  `json:"rps"`
	ErrorRPS   float64  `json:"error_rps"`
	ErrorRatio float64  `json:"error_ratio"`
	P50Seconds float64  `json:"p50_seconds"`
	P99Seconds float64  `json:"p99_seconds"`
	Hot        bool     `json:"hot"`
	HotReasons []string `json:"hot_reasons,omitempty"`
}

// Node is a service in the graph.
type Node struct {
	Name        string   `json:"name"`
	InboundRPS  float64  `json:"inbound_rps"`
	OutboundRPS float64  `json:"outbound_rps"`
	Callers     []string `json:"callers"`
	Callees     []string `json:"callees"`
	// BlastRadius lists every service that (transitively) calls this one and
	// is therefore impacted when it degrades.
	BlastRadius []string `json:"blast_radius"`
	// Entrypoint is true when nothing inside the mesh calls this service.
	Entrypoint bool `json:"entrypoint"`
}

// Graph is the analysed topology.
type Graph struct {
	Window string `json:"window"`
	Nodes  []Node `json:"nodes"`
	Edges  []Edge `json:"edges"`
}

// Thresholds mark edges as hot.
type Thresholds struct {
	ErrorRatio float64
	P99Seconds float64
}

// Analyzer queries Prometheus and builds Graphs.
type Analyzer struct {
	Q        prom.Querier
	Selector string // optional extra matcher, e.g. k8s_cluster_name="prod"
	T        Thresholds
}

func (a Analyzer) sel(extra string) string {
	parts := []string{}
	if a.Selector != "" {
		parts = append(parts, a.Selector)
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Build queries the service-graph metrics over window and analyses them.
func (a Analyzer) Build(ctx context.Context, window string) (*Graph, error) {
	q := func(promql string) ([]prom.Sample, error) { return a.Q.Query(ctx, promql) }
	rps, err := q(fmt.Sprintf("sum by (client, server) (rate(traces_service_graph_request_total%s[%s]))", a.sel(""), window))
	if err != nil {
		return nil, err
	}
	errs, err := q(fmt.Sprintf("sum by (client, server) (rate(traces_service_graph_request_failed_total%s[%s]))", a.sel(""), window))
	if err != nil {
		return nil, err
	}
	quant := func(p float64) ([]prom.Sample, error) {
		return q(fmt.Sprintf("histogram_quantile(%g, sum by (client, server, le) (rate(traces_service_graph_request_server_seconds_bucket%s[%s])))", p, a.sel(""), window))
	}
	p50, err := quant(0.5)
	if err != nil {
		return nil, err
	}
	p99, err := quant(0.99)
	if err != nil {
		return nil, err
	}
	return Analyze(window, rps, errs, p50, p99, a.T), nil
}

type key struct{ c, s string }

func index(samples []prom.Sample) map[key]float64 {
	m := map[key]float64{}
	for _, s := range samples {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		m[key{s.Labels["client"], s.Labels["server"]}] = s.Value
	}
	return m
}

// Analyze is the pure part of Build (unit-testable without Prometheus).
func Analyze(window string, rps, errs, p50, p99 []prom.Sample, t Thresholds) *Graph {
	g := &Graph{Window: window}
	errIdx, p50Idx, p99Idx := index(errs), index(p50), index(p99)
	nodes := map[string]*Node{}
	node := func(n string) *Node {
		if nodes[n] == nil {
			nodes[n] = &Node{Name: n}
		}
		return nodes[n]
	}
	for k, r := range index(rps) {
		// Skip stale series (rate 0) and unpaired spans that the connector
		// reports as the virtual "unknown" peer after its store TTL expires.
		if k.c == "" || k.s == "" || r <= 0 || k.s == "unknown" || k.c == "unknown" {
			continue
		}
		e := Edge{Client: k.c, Server: k.s, RPS: r, ErrorRPS: errIdx[k], P50Seconds: p50Idx[k], P99Seconds: p99Idx[k]}
		if r > 0 {
			e.ErrorRatio = math.Min(1, e.ErrorRPS/r)
		}
		if t.ErrorRatio > 0 && e.ErrorRatio > t.ErrorRatio {
			e.HotReasons = append(e.HotReasons, fmt.Sprintf("error ratio %.2f%% > %.2f%%", 100*e.ErrorRatio, 100*t.ErrorRatio))
		}
		if t.P99Seconds > 0 && e.P99Seconds > t.P99Seconds {
			e.HotReasons = append(e.HotReasons, fmt.Sprintf("p99 %.0fms > %.0fms", 1000*e.P99Seconds, 1000*t.P99Seconds))
		}
		e.Hot = len(e.HotReasons) > 0
		g.Edges = append(g.Edges, e)

		cn, sn := node(k.c), node(k.s)
		cn.OutboundRPS += r
		cn.Callees = append(cn.Callees, k.s)
		sn.InboundRPS += r
		sn.Callers = append(sn.Callers, k.c)
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].Client != g.Edges[j].Client {
			return g.Edges[i].Client < g.Edges[j].Client
		}
		return g.Edges[i].Server < g.Edges[j].Server
	})
	for _, n := range nodes {
		sort.Strings(n.Callers)
		sort.Strings(n.Callees)
		n.Entrypoint = len(n.Callers) == 0
		n.BlastRadius = upstreams(n.Name, nodes)
		g.Nodes = append(g.Nodes, *n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Name < g.Nodes[j].Name })
	return g
}

// upstreams walks caller edges breadth-first (cycle safe).
func upstreams(start string, nodes map[string]*Node) []string {
	seen := map[string]bool{start: true}
	queue := []string{start}
	out := []string{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range nodes[cur].Callers {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// HotEdges returns only unhealthy edges.
func (g *Graph) HotEdges() []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.Hot {
			out = append(out, e)
		}
	}
	return out
}

func id(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

// Mermaid renders the graph as a Mermaid flowchart (renders natively in
// GitHub, GitLab, Confluence and Notion). Hot edges are drawn in red.
func (g *Graph) Mermaid() string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "  %s[\"%s\"]\n", id(n.Name), n.Name)
	}
	var hot []int
	for i, e := range g.Edges {
		fmt.Fprintf(&b, "  %s -->|\"%.1f rps / %.1f%% err / p99 %.0fms\"| %s\n",
			id(e.Client), e.RPS, 100*e.ErrorRatio, 1000*e.P99Seconds, id(e.Server))
		if e.Hot {
			hot = append(hot, i)
		}
	}
	for _, i := range hot {
		fmt.Fprintf(&b, "  linkStyle %d stroke:#d62728,stroke-width:3px\n", i)
	}
	return b.String()
}

// DOT renders the graph for Graphviz.
func (g *Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph services {\n  rankdir=LR;\n  node [shape=box, style=rounded];\n")
	for _, e := range g.Edges {
		color := "black"
		if e.Hot {
			color = "red"
		}
		fmt.Fprintf(&b, "  %q -> %q [label=\"%.1f rps\\n%.1f%% err\\np99 %.0fms\", color=%s];\n",
			e.Client, e.Server, e.RPS, 100*e.ErrorRatio, 1000*e.P99Seconds, color)
	}
	b.WriteString("}\n")
	return b.String()
}
