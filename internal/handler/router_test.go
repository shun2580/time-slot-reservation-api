package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAuth は、Authorization ヘッダが "ok" のときだけ通す試験用のミドルウェア。通さないときは 401 を返す。
func fakeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "ok" {
			WriteStatusProblem(w, r, http.StatusUnauthorized, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
}

// newTestRouter は、healthz(Public)と /things(Protected)を持つ Router を組み上げる。
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	r := NewRouter()
	r.Public("GET /healthz", http.HandlerFunc(Healthz))
	r.Protected("GET /things/{id}", okHandler("thing"))
	r.Protected("POST /things", okHandler("created"))
	r.SetAuth(fakeAuth)
	h, err := r.Handler()
	if err != nil {
		t.Fatalf("Handler(): %v", err)
	}
	return h
}

func serve(h http.Handler, method, path, authz string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthz_ok(t *testing.T) {
	rr := serve(newTestRouter(t), http.MethodGet, "/healthz", "") // Authorization 無し
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

func TestHealthz_headIsAllowed(t *testing.T) {
	rr := serve(newTestRouter(t), http.MethodHead, "/healthz", "")
	if rr.Code != http.StatusOK {
		t.Errorf("HEAD /healthz: status = %d, want 200(GET の登録は HEAD も通す)", rr.Code)
	}
}

func TestRouter_unknownPathIs404Problem(t *testing.T) {
	h := newTestRouter(t)
	for _, path := range []string{"/", "/nope", "/slots/abc/unknown"} {
		for _, authz := range []string{"", "ok"} { // 認証の有無に関わらず、認証より前に 404
			rr := serve(h, http.MethodGet, path, authz)
			if rr.Code != http.StatusNotFound {
				t.Errorf("%s (authz=%q): status = %d, want 404", path, authz, rr.Code)
				continue
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("%s: Content-Type = %q", path, ct)
			}
			if rr.Header().Get("Allow") != "" {
				t.Errorf("%s: 404 に Allow は付けない: %q", path, rr.Header().Get("Allow"))
			}
			m := decodeProblem(t, rr)
			if m["status"] != float64(404) || m["type"] != "about:blank" {
				t.Errorf("%s: 本文が違う: %v", path, m)
			}
		}
	}
}

func TestRouter_methodMismatchIs405WithAllow(t *testing.T) {
	h := newTestRouter(t)
	tests := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodDelete, "/healthz", "GET, HEAD"},
		{http.MethodPut, "/things", "POST"},
		{http.MethodDelete, "/things/42", "GET, HEAD"},
	}
	for _, tc := range tests {
		for _, authz := range []string{"", "ok"} { // 405 も認証より前
			rr := serve(h, tc.method, tc.path, authz)
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s (authz=%q): status = %d, want 405", tc.method, tc.path, authz, rr.Code)
				continue
			}
			if got := rr.Header().Get("Allow"); got != tc.allow {
				t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("%s %s: Content-Type = %q", tc.method, tc.path, ct)
			}
			m := decodeProblem(t, rr)
			if m["type"] != "about:blank" || m["title"] != "Method Not Allowed" || m["status"] != float64(405) {
				t.Errorf("%s %s: 本文が違う: %v", tc.method, tc.path, m)
			}
		}
	}
}

func TestRouter_allowIsSortedAcrossRegistrations(t *testing.T) {
	r := NewRouter()
	r.Public("POST /x", okHandler("p"))
	r.Public("GET /x", okHandler("g"))
	r.Public("DELETE /x", okHandler("d"))
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	rr := serve(h, http.MethodPut, "/x", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "DELETE, GET, HEAD, POST" {
		t.Errorf("Allow = %q, want sorted \"DELETE, GET, HEAD, POST\"", got)
	}
}

func TestRouter_protectedPassesThroughAuth(t *testing.T) {
	h := newTestRouter(t)

	rr := serve(h, http.MethodGet, "/things/42", "ok")
	if rr.Code != http.StatusOK || rr.Body.String() != "thing" {
		t.Errorf("認証済み: status=%d body=%q, want 200 \"thing\"", rr.Code, rr.Body.String())
	}
	rr = serve(h, http.MethodPost, "/things", "ok")
	if rr.Code != http.StatusOK || rr.Body.String() != "created" {
		t.Errorf("認証済み POST: status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestRouter_protectedRejectedByAuthNeverReachesHandler(t *testing.T) {
	reached := false
	r := NewRouter()
	r.Protected("GET /secret", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r.SetAuth(fakeAuth)
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}

	rr := serve(h, http.MethodGet, "/secret", "") // 未認証
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if reached {
		t.Error("認証が 401 を返したのにハンドラに届いた")
	}
}

func TestRouter_publicHasNoAuth(t *testing.T) {
	authCalled := false
	r := NewRouter()
	r.Public("GET /open", okHandler("open"))
	r.Protected("GET /closed", okHandler("closed"))
	r.SetAuth(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			authCalled = true
			next.ServeHTTP(w, req)
		})
	})
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}

	rr := serve(h, http.MethodGet, "/open", "")
	if rr.Code != http.StatusOK || authCalled {
		t.Errorf("Public に認証が掛かった: status=%d authCalled=%v", rr.Code, authCalled)
	}
	serve(h, http.MethodGet, "/closed", "")
	if !authCalled {
		t.Error("Protected に認証が掛からなかった")
	}
}

func TestRouter_authIsNotCalledForUnmatchedRequests(t *testing.T) {
	authCalled := false
	r := NewRouter()
	r.Protected("GET /closed", okHandler("closed"))
	r.SetAuth(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			authCalled = true
			next.ServeHTTP(w, req)
		})
	})
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	serve(h, http.MethodGet, "/nope", "")
	serve(h, http.MethodPost, "/closed", "")
	if authCalled {
		t.Error("404・405 の要求に認証が掛かった")
	}
}

func TestRouter_pathValueIsAvailable(t *testing.T) {
	var got string
	r := NewRouter()
	r.Public("GET /things/{id}", http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		got = req.PathValue("id")
	}))
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	serve(h, http.MethodGet, "/things/abc", "")
	if got != "abc" {
		t.Errorf("PathValue(id) = %q, want abc", got)
	}
}

func TestRouter_handlerErrorsInsteadOfPanic(t *testing.T) {
	t.Run("Protected があるのに SetAuth が無い", func(t *testing.T) {
		r := NewRouter()
		r.Protected("GET /closed", okHandler("x"))
		h, err := r.Handler()
		if err == nil || h != nil {
			t.Errorf("エラーを返すはず: h=%v err=%v", h, err)
		}
	})
	t.Run("Public だけなら SetAuth 無しでよい", func(t *testing.T) {
		r := NewRouter()
		r.Public("GET /open", okHandler("x"))
		if _, err := r.Handler(); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("pattern の重複", func(t *testing.T) {
		r := NewRouter()
		r.Public("GET /dup", okHandler("a"))
		r.Public("GET /dup", okHandler("b"))
		if _, err := r.Handler(); err == nil {
			t.Error("エラーを返すはず")
		}
	})
	t.Run("不正な pattern", func(t *testing.T) {
		r := NewRouter()
		r.Public("GET /{", okHandler("a"))
		if _, err := r.Handler(); err == nil {
			t.Error("エラーを返すはず")
		}
	})
	t.Run("ハンドラが nil", func(t *testing.T) {
		r := NewRouter()
		r.Public("GET /nil", nil)
		if _, err := r.Handler(); err == nil {
			t.Error("エラーを返すはず")
		}
	})
}
