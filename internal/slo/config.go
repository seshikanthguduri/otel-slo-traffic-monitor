// Package slo evaluates Service Level Objectives and Service Level Agreements
// from Prometheus-compatible metrics produced by OpenTelemetry.
//
// Concepts follow the Google SRE Workbook ("Implementing SLOs" and "Alerting
// on SLOs"): SLI = good/valid events, error budget = 1 - objective, and
// multi-window, multi-burn-rate alerting. SLAs are modelled as a looser,
// contractual target on the same SLI with service-credit tiers.
package slo

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/containers"
)

// WindowPlaceholder is replaced by a PromQL range (e.g. 5m, 1h, 30d).
const WindowPlaceholder = "{{window}}"

// Config is the root of slo-config.yaml.
type Config struct {
	Prometheus struct {
		URL     string            `yaml:"url"`
		Headers map[string]string `yaml:"headers"`
	} `yaml:"prometheus"`
	EvaluationInterval Duration           `yaml:"evaluationInterval"`
	AlertPolicies      []AlertPolicy      `yaml:"alertPolicies"`
	Traffic            TrafficConfig      `yaml:"traffic"`
	Containers         containers.Queries `yaml:"containers"`
	SLOs               []SLO              `yaml:"slos"`
}

// TrafficConfig tunes the service-graph analysis.
type TrafficConfig struct {
	Window              string  `yaml:"window"`
	ErrorRatioThreshold float64 `yaml:"errorRatioThreshold"`
	P99ThresholdSeconds float64 `yaml:"p99ThresholdSeconds"`
}

// SLO is a single objective.
type SLO struct {
	Name        string  `yaml:"name"`
	Service     string  `yaml:"service"`
	Description string  `yaml:"description"`
	Objective   float64 `yaml:"objective"` // e.g. 0.999
	Window      string  `yaml:"window"`    // rolling window, e.g. 30d
	SLI         SLI     `yaml:"sli"`
	SLA         *SLA    `yaml:"sla,omitempty"`
}

// SLI describes how to count bad and total events. Either use a template or
// provide errorQuery/totalQuery containing {{window}}.
type SLI struct {
	// Template: http-availability | http-latency | servicegraph-edge-availability | custom
	Template string `yaml:"template"`
	// Metric is the base histogram name for http-* templates.
	Metric string `yaml:"metric"`
	// Selector is appended to the label matcher, e.g. k8s_namespace_name="demo".
	Selector string `yaml:"selector"`
	// LatencyThreshold (seconds) must equal a histogram bucket boundary ("le").
	LatencyThreshold string `yaml:"latencyThreshold"`
	// Client/Server identify a service-graph edge.
	Client string `yaml:"client"`
	Server string `yaml:"server"`

	ErrorQuery string `yaml:"errorQuery"`
	TotalQuery string `yaml:"totalQuery"`
}

// SLA is a contractual commitment layered on top of an SLO.
type SLA struct {
	Target  float64      `yaml:"target"` // e.g. 0.995; must be <= objective
	Credits []CreditTier `yaml:"credits"`
}

// CreditTier grants Percent service credit when SLI < Below.
type CreditTier struct {
	Below   float64 `yaml:"below"`
	Percent float64 `yaml:"percent"`
}

// AlertPolicy is one row of a multi-window, multi-burn-rate policy.
// BurnRate is derived from BudgetConsumed when omitted:
// burnRate = budgetConsumed * sloWindow / longWindow.
type AlertPolicy struct {
	Severity       string  `yaml:"severity"`
	LongWindow     string  `yaml:"longWindow"`
	ShortWindow    string  `yaml:"shortWindow"`
	BudgetConsumed float64 `yaml:"budgetConsumed"`
	BurnRate       float64 `yaml:"burnRate"`
}

// DefaultAlertPolicies are the SRE Workbook recommendations
// (2% budget in 1h, 5% in 6h, 10% in 1d, 10% in 3d). For a 30d window they
// produce the familiar 14.4 / 6 / 3 / 1 burn-rate thresholds.
func DefaultAlertPolicies() []AlertPolicy {
	return []AlertPolicy{
		{Severity: "page", LongWindow: "1h", ShortWindow: "5m", BudgetConsumed: 0.02},
		{Severity: "page", LongWindow: "6h", ShortWindow: "30m", BudgetConsumed: 0.05},
		{Severity: "ticket", LongWindow: "1d", ShortWindow: "2h", BudgetConsumed: 0.10},
		{Severity: "ticket", LongWindow: "3d", ShortWindow: "6h", BudgetConsumed: 0.10},
	}
}

// Duration is a YAML-friendly time.Duration that also accepts d/w units.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseWindow(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

var windowRE = regexp.MustCompile(`^([0-9]+)(ms|s|m|h|d|w|y)$`)

// ParseWindow parses a single-unit Prometheus duration such as 5m, 6h, 30d.
func ParseWindow(s string) (time.Duration, error) {
	m := windowRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid window %q (use a Prometheus duration like 5m, 1h, 30d)", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{
		"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour,
	}[m[2]]
	if n <= 0 {
		return 0, fmt.Errorf("window %q must be positive", s)
	}
	return time.Duration(n) * unit, nil
}

// LoadConfig reads, defaults and validates a config file. PROMETHEUS_URL
// overrides prometheus.url so one config works across clusters/clouds.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses YAML bytes (see LoadConfig).
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if u := os.Getenv("PROMETHEUS_URL"); u != "" {
		c.Prometheus.URL = u
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Prometheus.URL == "" {
		c.Prometheus.URL = "http://localhost:9090"
	}
	if c.EvaluationInterval == 0 {
		c.EvaluationInterval = Duration(30 * time.Second)
	}
	if len(c.AlertPolicies) == 0 {
		c.AlertPolicies = DefaultAlertPolicies()
	}
	if c.Traffic.Window == "" {
		c.Traffic.Window = "5m"
	}
	if c.Traffic.ErrorRatioThreshold == 0 {
		c.Traffic.ErrorRatioThreshold = 0.01
	}
	if c.Traffic.P99ThresholdSeconds == 0 {
		c.Traffic.P99ThresholdSeconds = 0.5
	}
	c.Containers = c.Containers.WithDefaults()
	for i := range c.SLOs {
		if c.SLOs[i].SLI.Metric == "" {
			c.SLOs[i].SLI.Metric = "http_server_request_duration_seconds"
		}
		if c.SLOs[i].Window == "" {
			c.SLOs[i].Window = "30d"
		}
	}
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks the whole config and returns all problems at once.
func (c *Config) Validate() error {
	var errs []error
	if _, err := ParseWindow(c.Traffic.Window); err != nil {
		errs = append(errs, fmt.Errorf("traffic.window: %w", err))
	}
	for i, p := range c.AlertPolicies {
		l, err1 := ParseWindow(p.LongWindow)
		s, err2 := ParseWindow(p.ShortWindow)
		if err := errors.Join(err1, err2); err != nil {
			errs = append(errs, fmt.Errorf("alertPolicies[%d]: %w", i, err))
			continue
		}
		if s >= l {
			errs = append(errs, fmt.Errorf("alertPolicies[%d]: shortWindow must be shorter than longWindow", i))
		}
		if p.BurnRate <= 0 && (p.BudgetConsumed <= 0 || p.BudgetConsumed > 1) {
			errs = append(errs, fmt.Errorf("alertPolicies[%d]: set burnRate > 0 or budgetConsumed in (0,1]", i))
		}
		if p.Severity == "" {
			errs = append(errs, fmt.Errorf("alertPolicies[%d]: severity is required", i))
		}
	}
	seen := map[string]bool{}
	for i, s := range c.SLOs {
		at := fmt.Sprintf("slos[%d] (%s)", i, s.Name)
		if !nameRE.MatchString(s.Name) {
			errs = append(errs, fmt.Errorf("%s: name must be DNS-label style", at))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", at))
		}
		seen[s.Name] = true
		if s.Objective <= 0 || s.Objective >= 1 {
			errs = append(errs, fmt.Errorf("%s: objective must be in (0,1), e.g. 0.999", at))
		}
		if _, err := ParseWindow(s.Window); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
		}
		if _, _, err := s.Queries(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", at, err))
		}
		if s.SLA != nil {
			if s.SLA.Target <= 0 || s.SLA.Target >= 1 {
				errs = append(errs, fmt.Errorf("%s: sla.target must be in (0,1)", at))
			}
			if s.SLA.Target > s.Objective {
				errs = append(errs, fmt.Errorf("%s: sla.target (%v) must not be stricter than the SLO objective (%v); the internal SLO is the early-warning buffer", at, s.SLA.Target, s.Objective))
			}
			for j, t := range s.SLA.Credits {
				if t.Below <= 0 || t.Below > 1 || t.Percent < 0 || t.Percent > 100 {
					errs = append(errs, fmt.Errorf("%s: sla.credits[%d] invalid", at, j))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Windows returns every PromQL window the SLO needs, sorted by duration.
func (c *Config) Windows(s SLO) []string {
	set := map[string]time.Duration{}
	add := func(w string) {
		d, _ := ParseWindow(w)
		set[w] = d
	}
	add(s.Window)
	for _, p := range c.AlertPolicies {
		add(p.LongWindow)
		add(p.ShortWindow)
	}
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return set[out[i]] < set[out[j]] })
	return out
}

// BurnRateThreshold resolves the burn-rate threshold of a policy for an SLO.
func BurnRateThreshold(p AlertPolicy, sloWindow string) float64 {
	if p.BurnRate > 0 {
		return p.BurnRate
	}
	w, _ := ParseWindow(sloWindow)
	l, _ := ParseWindow(p.LongWindow)
	return p.BudgetConsumed * float64(w) / float64(l)
}

// Queries returns the (errorQuery, totalQuery) templates for the SLI.
func (s SLO) Queries() (string, string, error) {
	sli := s.SLI
	sel := func(extra string) string {
		parts := []string{}
		if s.Service != "" {
			parts = append(parts, fmt.Sprintf(`service_name=%q`, s.Service))
		}
		if sli.Selector != "" {
			parts = append(parts, sli.Selector)
		}
		if extra != "" {
			parts = append(parts, extra)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	w := "[" + WindowPlaceholder + "]"
	switch sli.Template {
	case "http-availability":
		if s.Service == "" {
			return "", "", errors.New("http-availability requires service")
		}
		total := fmt.Sprintf("sum(rate(%s_count%s%s))", sli.Metric, sel(""), w)
		bad := fmt.Sprintf(`(sum(rate(%s_count%s%s)) or vector(0))`, sli.Metric, sel(`http_response_status_code=~"5.."`), w)
		return bad, total, nil
	case "http-latency":
		if s.Service == "" || sli.LatencyThreshold == "" {
			return "", "", errors.New("http-latency requires service and latencyThreshold")
		}
		if _, err := strconv.ParseFloat(sli.LatencyThreshold, 64); err != nil {
			return "", "", fmt.Errorf("latencyThreshold: %w", err)
		}
		total := fmt.Sprintf("sum(rate(%s_count%s%s))", sli.Metric, sel(""), w)
		good := fmt.Sprintf("sum(rate(%s_bucket%s%s))", sli.Metric, sel(fmt.Sprintf(`le=%q`, sli.LatencyThreshold)), w)
		return "(" + total + " - " + good + ")", total, nil
	case "servicegraph-edge-availability":
		if sli.Client == "" || sli.Server == "" {
			return "", "", errors.New("servicegraph-edge-availability requires client and server")
		}
		m := fmt.Sprintf(`client=%q,server=%q`, sli.Client, sli.Server)
		if sli.Selector != "" {
			m += "," + sli.Selector
		}
		total := fmt.Sprintf("sum(rate(traces_service_graph_request_total{%s}%s))", m, w)
		bad := fmt.Sprintf("(sum(rate(traces_service_graph_request_failed_total{%s}%s)) or vector(0))", m, w)
		return bad, total, nil
	case "custom", "":
		if !strings.Contains(sli.ErrorQuery, WindowPlaceholder) || !strings.Contains(sli.TotalQuery, WindowPlaceholder) {
			return "", "", errors.New("custom SLI needs errorQuery and totalQuery containing " + WindowPlaceholder)
		}
		return sli.ErrorQuery, sli.TotalQuery, nil
	default:
		return "", "", fmt.Errorf("unknown sli.template %q", sli.Template)
	}
}

// Render substitutes the window into a query template.
func Render(tmpl, window string) string {
	return strings.ReplaceAll(tmpl, WindowPlaceholder, window)
}
