package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// stub500 は、handler 層に依存しないための onPanic の代役。
func stub500(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"status":500}`))
}

func TestRecover_panicBecomes500AndIsLoggedWithRequestID(t *testing.T) {
	logger, buf := newTestLogger()
	h := RequestID(Logging(logger)(Recover(stub500)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "req-3")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("onPanic の応答が使われていない: %q", rr.Header().Get("Content-Type"))
	}

	lines := parseLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("panic ログとアクセスログの2行のはず: %d行\n%s", len(lines), buf.String())
	}
	if lines[0]["msg"] != "panic recovered" || lines[0]["request_id"] != "req-3" || lines[0]["panic"] != "boom" {
		t.Errorf("panic ログが不足: %v", lines[0])
	}
	if lines[0]["level"] != "ERROR" {
		t.Errorf("panic ログが ERROR でない: %v", lines[0]["level"])
	}
	if lines[1]["msg"] != "access" || lines[1]["status"] != float64(500) {
		t.Errorf("アクセスログの status が 500 でない: %v", lines[1])
	}
}

func TestRecover_noPanicPassesThrough(t *testing.T) {
	called := false
	h := Recover(func(http.ResponseWriter, *http.Request) { called = true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusCreated || called {
		t.Fatalf("panic が無いのに介入した: code=%d onPanic called=%v", rr.Code, called)
	}
}

func TestRecover_skipsOnPanicAfterResponseStarted(t *testing.T) {
	called := false
	h := Recover(func(http.ResponseWriter, *http.Request) { called = true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("after header")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if called {
		t.Fatal("応答を書き始めた後なのに onPanic を呼んだ")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(先に書いた値のまま)", rr.Code)
	}
}

func TestRecover_repanicsErrAbortHandler(t *testing.T) {
	h := Recover(stub500)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("http.ErrAbortHandler を再 panic するべき: got %v", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("再 panic されなかった")
}
