package handler

import (
	"net/http"
	"testing"
)

var reservationStubRoutes = []struct{ method, path string }{
	{http.MethodPost, "/slots/abc/reservations"},
	{http.MethodGet, "/reservations"},
	{http.MethodGet, "/reservations/abc"},
	{http.MethodPost, "/reservations/abc/cancel"},
	{http.MethodGet, "/slots/abc/reservations"},
}

func newStubRouter(t *testing.T) http.Handler {
	t.Helper()
	r := NewRouter()
	r.Public("GET /healthz", http.HandlerFunc(Healthz))
	RegisterReservationStubs(r)
	r.SetAuth(fakeAuth)
	h, err := r.Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	return h
}

func TestReservationStubs_authenticatedGets501Problem(t *testing.T) {
	h := newStubRouter(t)
	for _, rt := range reservationStubRoutes {
		rr := serve(h, rt.method, rt.path, "ok")
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want 501", rt.method, rt.path, rr.Code)
			continue
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s: Content-Type = %q", rt.method, rt.path, ct)
		}
		m := decodeProblem(t, rr)
		if m["type"] != "about:blank" || m["title"] != "Not Implemented" || m["status"] != float64(501) {
			t.Errorf("%s %s: 本文が違う: %v", rt.method, rt.path, m)
		}
	}
}

func TestReservationStubs_unauthenticatedGets401NotReaching501(t *testing.T) {
	h := newStubRouter(t)
	for _, rt := range reservationStubRoutes {
		rr := serve(h, rt.method, rt.path, "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401(認証が先)", rt.method, rt.path, rr.Code)
		}
	}
}

func TestReservationStubs_wrongMethodIs405(t *testing.T) {
	h := newStubRouter(t)
	rr := serve(h, http.MethodDelete, "/reservations/abc", "ok")
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("status=%d Allow=%q, want 405 \"GET, HEAD\"", rr.Code, rr.Header().Get("Allow"))
	}
}
