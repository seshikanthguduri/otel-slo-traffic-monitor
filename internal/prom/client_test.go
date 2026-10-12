package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeProm(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.FormValue("query") == "" {
			t.Errorf("unexpected request %s %q", r.URL.Path, r.FormValue("query"))
		}
		if r.Header.Get("X-Scope-OrgID") != "tenant-a" {
			t.Errorf("tenant header not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestQueryVector(t *testing.T) {
	srv := fakeProm(t, `{"status":"success","data":{"resultType":"vector","result":[
	 {"metric":{"client":"frontend","server":"checkout"},"value":[1700000000.1,"12.5"]},
	 {"metric":{"client":"checkout","server":"payment"},"value":[1700000000.1,"NaN"]}]}}`)
	defer srv.Close()
	c := NewClient(srv.URL+"/", nil)
	c.Header.Set("X-Scope-OrgID", "tenant-a")
	got, err := c.Query(context.Background(), "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != 12.5 || got[0].Labels["server"] != "checkout" {
		t.Fatalf("unexpected %+v", got)
	}
	if _, err := Scalar(context.Background(), c, "up"); err == nil {
		t.Fatal("expected error for multi-series scalar")
	}
}

func TestQueryScalarAndErrors(t *testing.T) {
	srv := fakeProm(t, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0.25"]}}`)
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	c.Header.Set("X-Scope-OrgID", "tenant-a")
	v, err := Scalar(context.Background(), c, "vector(0.25)")
	if err != nil || v != 0.25 {
		t.Fatalf("got %v %v", v, err)
	}

	bad := fakeProm(t, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	defer bad.Close()
	c2 := NewClient(bad.URL, nil)
	c2.Header.Set("X-Scope-OrgID", "tenant-a")
	if _, err := c2.Query(context.Background(), "sum("); err == nil {
		t.Fatal("expected API error")
	}

	empty := fakeProm(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	defer empty.Close()
	c3 := NewClient(empty.URL, nil)
	c3.Header.Set("X-Scope-OrgID", "tenant-a")
	if _, err := Scalar(context.Background(), c3, "x"); !errors.Is(err, ErrNoData) {
		t.Fatalf("want ErrNoData, got %v", err)
	}
}
