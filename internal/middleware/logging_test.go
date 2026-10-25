package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

// parseLines は、JSON Lines のログを1行ずつ map にする。
func parseLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("ログがJSONでない: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLogging_accessLogFields(t *testing.T) {
	logger, buf := newTestLogger()
	h := RequestID(Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))

	req := httptest.NewRequest(http.MethodPost, "/slots/abc?email=taro@example.com&token=secret", nil)
	req.Header.Set(RequestIDHeader, "req-1")
	req.Header.Set("Authorization", "Bearer super-secret-token")
	req.Header.Set("X-Email", "hanako@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	lines := parseLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("アクセスログは1行のはず: %d行\n%s", len(lines), buf.String())
	}
	got := lines[0]
	if got["method"] != "POST" || got["path"] != "/slots/abc" {
		t.Errorf("method/path が違う: %v / %v", got["method"], got["path"])
	}
	if got["status"] != float64(http.StatusTeapot) {
		t.Errorf("status が違う: %v", got["status"])
	}
	if got["request_id"] != "req-1" {
		t.Errorf("request_id が違う: %v", got["request_id"])
	}
	if d, ok := got["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms が数値でない/負: %v", got["duration_ms"])
	}
	for _, leaked := range []string{"taro@example.com", "hanako@example.com", "super-secret-token", "token=secret", "email="} {
		if strings.Contains(buf.String(), leaked) {
			t.Errorf("ログに出してはならない値が含まれた: %q\n%s", leaked, buf.String())
		}
	}
}

func TestLogging_statusDefaultsTo200(t *testing.T) {
	logger, buf := newTestLogger()
	h := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok")) // WriteHeader を呼ばない
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	lines := parseLines(t, buf)
	if len(lines) != 1 || lines[0]["status"] != float64(200) {
		t.Fatalf("WriteHeader 無しの status は 200 のはず: %s", buf.String())
	}
}

func TestLoggerFrom_carriesRequestID(t *testing.T) {
	logger, buf := newTestLogger()
	h := RequestID(Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LoggerFrom(r.Context()).Info("業務ログ")
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "req-2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := parseLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("業務ログとアクセスログの2行のはず: %d行\n%s", len(lines), buf.String())
	}
	if lines[0]["msg"] != "業務ログ" || lines[0]["request_id"] != "req-2" {
		t.Errorf("LoggerFrom のログに request_id が載っていない: %v", lines[0])
	}
}

func TestLoggerFrom_fallsBackToDefault(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if LoggerFrom(req.Context()) == nil {
		t.Fatal("logger が無いとき nil を返した(slog.Default() を返すべき)")
	}
}
