package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_ok(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil) // Authorization 無し
	NewRouter().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if body := rr.Body.String(); body != `{"status":"ok"}` {
		t.Errorf("body = %q", body)
	}
}

func TestRouter_unknownPathIs404Problem(t *testing.T) {
	for _, path := range []string{"/", "/nope", "/slots/abc/unknown"} {
		rr := httptest.NewRecorder()
		NewRouter().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: Content-Type = %q", path, ct)
		}
		m := decodeProblem(t, rr)
		if m["status"] != float64(404) || m["type"] != "about:blank" {
			t.Errorf("%s: 本文が違う: %v", path, m)
		}
	}
}
