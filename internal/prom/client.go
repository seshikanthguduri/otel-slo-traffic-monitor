// Package prom is a small, dependency-free client for the Prometheus HTTP
// query API (/api/v1/query). It works against any Prometheus-compatible
// backend: Prometheus, Amazon Managed Service for Prometheus (behind a SigV4
// proxy), Google Managed Prometheus, Azure Monitor managed Prometheus,
// Thanos, Cortex, Mimir or VictoriaMetrics.
package prom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sample is a single instant-vector element.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Querier is the interface the SLO engine depends on (easy to fake in tests).
type Querier interface {
	Query(ctx context.Context, promql string) ([]Sample, error)
}

// Client implements Querier over HTTP.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Header is added to every request (e.g. tenant headers for Mimir/Cortex
	// "X-Scope-OrgID", or "Authorization").
	Header http.Header
}

// NewClient returns a Client with a sane timeout.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient, Header: http.Header{}}
}

type apiResponse struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

type queryData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type vectorItem struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// Query runs an instant query and returns vector or scalar results.
func (c *Client) Query(ctx context.Context, promql string) ([]Sample, error) {
	form := url.Values{"query": {promql}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var ar apiResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("prometheus: HTTP %d, undecodable body: %w", res.StatusCode, err)
	}
	if ar.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", ar.ErrorType, ar.Error)
	}
	return decodeData(ar.Data)
}

func decodeData(raw json.RawMessage) ([]Sample, error) {
	var qd queryData
	if err := json.Unmarshal(raw, &qd); err != nil {
		return nil, err
	}
	switch qd.ResultType {
	case "vector":
		var items []vectorItem
		if err := json.Unmarshal(qd.Result, &items); err != nil {
			return nil, err
		}
		out := make([]Sample, 0, len(items))
		for _, it := range items {
			v, err := parseValue(it.Value[1])
			if err != nil {
				return nil, err
			}
			out = append(out, Sample{Labels: it.Metric, Value: v})
		}
		return out, nil
	case "scalar":
		var pair [2]any
		if err := json.Unmarshal(qd.Result, &pair); err != nil {
			return nil, err
		}
		v, err := parseValue(pair[1])
		if err != nil {
			return nil, err
		}
		return []Sample{{Labels: map[string]string{}, Value: v}}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %q (use an instant vector/scalar query)", qd.ResultType)
	}
}

func parseValue(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, errors.New("sample value is not a string")
	}
	return strconv.ParseFloat(s, 64)
}

// ErrNoData is returned by Scalar when the query produced no usable sample.
var ErrNoData = errors.New("no data")

// Scalar runs a query expected to return exactly one finite number.
func Scalar(ctx context.Context, q Querier, promql string) (float64, error) {
	s, err := q.Query(ctx, promql)
	if err != nil {
		return 0, err
	}
	if len(s) == 0 || math.IsNaN(s[0].Value) || math.IsInf(s[0].Value, 0) {
		return 0, ErrNoData
	}
	if len(s) > 1 {
		return 0, fmt.Errorf("expected 1 series, got %d (aggregate with sum())", len(s))
	}
	return s[0].Value, nil
}
