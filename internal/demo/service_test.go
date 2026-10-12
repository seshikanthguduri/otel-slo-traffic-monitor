package demo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newSvc(t *testing.T, cfg Config, r float64) *Service {
	t.Helper()
	s, err := New(cfg, func() float64 { return r })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, workResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var wr workResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &wr)
	return rec, wr
}

func TestWorkSuccessWithDownstreams(t *testing.T) {
	leaf := newSvc(t, Config{Name: "payment"}, 0.99)
	srv := httptest.NewServer(leaf.Handler())
	defer srv.Close()

	root := newSvc(t, Config{Name: "checkout", Downstreams: []string{srv.URL + "/api/work"}}, 0.99)
	rec, wr := get(t, root.Handler(), "/api/work")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if len(wr.Downstreams) != 1 || wr.Downstreams[0].Status != 200 {
		t.Fatalf("unexpected downstreams: %+v", wr.Downstreams)
	}
}

func TestDownstreamFailurePropagatesAs502(t *testing.T) {
	leaf := newSvc(t, Config{Name: "payment", Fault: Fault{ErrorRate: 1}}, 0)
	srv := httptest.NewServer(leaf.Handler())
	defer srv.Close()

	root := newSvc(t, Config{Name: "checkout", Downstreams: []string{srv.URL + "/api/work"}}, 0.99)
	rec, wr := get(t, root.Handler(), "/api/work")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if !strings.Contains(wr.Error, "dependency failed") {
		t.Fatalf("unexpected error: %q", wr.Error)
	}
}

func TestInjectedErrorAndRuntimeFaultChange(t *testing.T) {
	s := newSvc(t, Config{Name: "inventory"}, 0.5)
	h := s.Handler()
	if rec, _ := get(t, h, "/api/work"); rec.Code != 200 {
		t.Fatalf("want 200 before fault, got %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/fault?error_rate=0.9", nil))
	if rec.Code != 200 {
		t.Fatalf("set fault: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := get(t, h, "/api/work"); rec.Code != 500 {
		t.Fatalf("want 500 after fault, got %d", rec.Code)
	}
}

func TestFaultValidation(t *testing.T) {
	s := newSvc(t, Config{Name: "x"}, 0)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/fault?error_rate=2", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestParseDownstreamsAndHost(t *testing.T) {
	got := ParseDownstreams(" http://a:8080/x, ,http://b/y ")
	if len(got) != 2 || got[0] != "http://a:8080/x" {
		t.Fatalf("got %v", got)
	}
	if hostOf("http://payment.demo.svc:8080/api") != "payment.demo.svc" {
		t.Fatal("hostOf")
	}
}
