package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
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

func TestBuildHandler_requestIDOnResponseAndAccessLog(t *testing.T) {
	logger, buf := testLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := buildHandler(inner, logger)

	req := httptest.NewRequest(http.MethodGet, "/x?q=1", nil)
	req.Header.Set("X-Request-ID", "req-main-1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-ID"); got != "req-main-1" {
		t.Errorf("X-Request-ID = %q", got)
	}
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["request_id"] != "req-main-1" || lines[0]["status"] != float64(204) {
		t.Fatalf("アクセスログが違う: %s", buf.String())
	}
}

func TestBuildHandler_panicBecomes500Problem(t *testing.T) {
	logger, buf := testLogger()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := buildHandler(inner, logger)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", "req-main-2")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("本文がJSONでない: %v", err)
	}
	if p["status"] != float64(500) || p["type"] != "about:blank" {
		t.Errorf("本文が違う: %v", p)
	}
	if strings.Contains(rr.Body.String(), "boom") {
		t.Errorf("panic の内容を応答に出してはならない: %s", rr.Body.String())
	}
	if got := rr.Header().Get("X-Request-ID"); got != "req-main-2" {
		t.Errorf("X-Request-ID = %q", got)
	}

	lines := logLines(t, buf)
	if len(lines) != 2 || lines[0]["msg"] != "panic recovered" || lines[0]["request_id"] != "req-main-2" {
		t.Fatalf("panic ログに request_id が載っていない: %s", buf.String())
	}
	if lines[1]["msg"] != "access" || lines[1]["status"] != float64(500) {
		t.Errorf("アクセスログの status が 500 でない: %v", lines[1])
	}
}

func TestRun_missingEnvNamesKeysButNoValues(t *testing.T) {
	logger, _ := testLogger()
	env := map[string]string{"APP_DB_PASSWORD": "should-never-appear"}
	err := run(context.Background(), func(k string) string { return env[k] }, logger)
	if err == nil {
		t.Fatal("必須の鍵が欠けているのに成功した")
	}
	if !strings.Contains(err.Error(), "APP_HTTP_ADDR") {
		t.Errorf("欠けた鍵名がエラーに無い: %v", err)
	}
	if strings.Contains(err.Error(), "should-never-appear") || strings.Contains(err.Error(), "APP_DB_PASSWORD") {
		t.Errorf("設定済みの鍵・値がエラーに混じった: %v", err)
	}
}

func TestRun_gracefulShutdownOnContextCancel(t *testing.T) {
	logger, _ := testLogger()
	env := map[string]string{
		"APP_HTTP_ADDR":         "127.0.0.1:0",
		"APP_DB_HOST":           "db",
		"APP_DB_PORT":           "3306",
		"APP_DB_USER":           "app",
		"APP_DB_PASSWORD":       "dummy-password",
		"APP_DB_NAME":           "reservation",
		"APP_KEYCLOAK_ISSUER":   "http://keycloak:8080/realms/test",
		"APP_KEYCLOAK_JWKS_URL": "http://keycloak:8080/realms/test/protocol/openid-connect/certs",
		"APP_JWT_AUDIENCE":      "reservation-api",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, logger) }()

	time.Sleep(200 * time.Millisecond) // listen に入る猶予
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown が成功しなかった: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx を取り消しても run が戻らない")
	}
}
