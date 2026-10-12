// Package api exposes SLO/SLA status, the service traffic graph and the
// container health view over a small JSON/text HTTP API.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/containers"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/slo"
	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/traffic"
)

// Server wires the handlers.
type Server struct {
	Engine        *slo.Engine
	Traffic       traffic.Analyzer
	Containers    containers.Collector
	TrafficWindow string
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, otelhttp.NewHandler(h, pattern))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Engine.Ready() {
			http.Error(w, "first evaluation pending", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	handle("GET /api/v1/slos", s.slos)
	handle("GET /api/v1/slos/{name}", s.sloByName)
	handle("GET /api/v1/sla", s.sla)
	handle("GET /api/v1/traffic", s.traffic)
	handle("GET /api/v1/traffic/mermaid", s.trafficText(func(g *traffic.Graph) string { return g.Mermaid() }))
	handle("GET /api/v1/traffic/dot", s.trafficText(func(g *traffic.Graph) string { return g.DOT() }))
	handle("GET /api/v1/containers", s.containers)
	handle("GET /api/v1/summary", s.summary)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) window(r *http.Request) string {
	if w := r.URL.Query().Get("window"); w != "" {
		if _, err := slo.ParseWindow(w); err == nil {
			return w
		}
	}
	return s.TrafficWindow
}

func (s *Server) slos(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.Snapshot())
}

func (s *Server) sloByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, st := range s.Engine.Snapshot() {
		if st.Name == name {
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "slo not found: " + name})
}

// SLAReport is a contract-facing summary.
type SLAReport struct {
	Name        string        `json:"name"`
	Service     string        `json:"service"`
	Window      string        `json:"window"`
	SLI         float64       `json:"sli"`
	Objective   float64       `json:"internal_objective"`
	SLA         slo.SLAStatus `json:"sla"`
	GeneratedAt time.Time     `json:"generated_at"`
}

func (s *Server) sla(w http.ResponseWriter, _ *http.Request) {
	var out []SLAReport
	for _, st := range s.Engine.Snapshot() {
		if st.SLA == nil {
			continue
		}
		out = append(out, SLAReport{Name: st.Name, Service: st.Service, Window: st.Window, SLI: st.SLI,
			Objective: st.Objective, SLA: *st.SLA, GeneratedAt: st.EvaluatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) traffic(w http.ResponseWriter, r *http.Request) {
	g, err := s.Traffic.Build(r.Context(), s.window(r))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) trafficText(render func(*traffic.Graph) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, err := s.Traffic.Build(r.Context(), s.window(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(render(g)))
	}
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	st, err := s.Containers.Collect(r.Context(), s.window(r))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// Summary is the one-call "how are we doing" view for dashboards and chatops.
type Summary struct {
	SLOsTotal     int            `json:"slos_total"`
	SLOsWithData  int            `json:"slos_with_data"`
	SLOsBreached  []string       `json:"slos_breached"`
	PagesFiring   []string       `json:"pages_firing"`
	TicketsFiring []string       `json:"tickets_firing"`
	SLAsAtRisk    []string       `json:"slas_at_risk"`
	SLAsBreached  []string       `json:"slas_breached"`
	HotEdges      []traffic.Edge `json:"hot_edges"`
	SaturatedPods []string       `json:"saturated_containers"`
	Errors        []string       `json:"errors,omitempty"`
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	sum := Summary{SLOsBreached: []string{}, PagesFiring: []string{}, TicketsFiring: []string{},
		SLAsAtRisk: []string{}, SLAsBreached: []string{}, HotEdges: []traffic.Edge{}, SaturatedPods: []string{}}
	for _, st := range s.Engine.Snapshot() {
		sum.SLOsTotal++
		if !st.HasData {
			continue
		}
		sum.SLOsWithData++
		if st.SLI < st.Objective {
			sum.SLOsBreached = append(sum.SLOsBreached, st.Name)
		}
		if st.Firing("page") {
			sum.PagesFiring = append(sum.PagesFiring, st.Name)
		}
		if st.Firing("ticket") {
			sum.TicketsFiring = append(sum.TicketsFiring, st.Name)
		}
		if st.SLA != nil {
			switch st.SLA.State {
			case slo.SLAAtRisk:
				sum.SLAsAtRisk = append(sum.SLAsAtRisk, st.Name)
			case slo.SLABreached:
				sum.SLAsBreached = append(sum.SLAsBreached, st.Name)
			}
		}
	}
	if g, err := s.Traffic.Build(r.Context(), s.window(r)); err == nil {
		if h := g.HotEdges(); h != nil {
			sum.HotEdges = h
		}
	} else {
		sum.Errors = append(sum.Errors, "traffic: "+err.Error())
	}
	if cs, err := s.Containers.Collect(r.Context(), s.window(r)); err == nil {
		for _, c := range cs {
			if c.Saturated {
				sum.SaturatedPods = append(sum.SaturatedPods, c.Namespace+"/"+c.Pod+"/"+c.Container)
			}
		}
	} else {
		sum.Errors = append(sum.Errors, "containers: "+err.Error())
	}
	writeJSON(w, http.StatusOK, sum)
}
