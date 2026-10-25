package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
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
	logger, buf := testLogger()
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
	// DB は存在しないが、起動も停止も妨げない。DSN(パスワードを含む)はログに出ない。
	if strings.Contains(buf.String(), "dummy-password") {
		t.Errorf("パスワードがログに出た: %s", buf.String())
	}
}

func TestNewServer_hasTimeouts(t *testing.T) {
	srv := newServer(http.NotFoundHandler())
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 10 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 30 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 120 * time.Second},
	}
	for _, c := range checks {
		if c.got <= 0 {
			t.Errorf("%s が未設定", c.name)
		}
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if srv.Handler == nil {
		t.Error("Handler が未設定")
	}
}

// fakeSlots は handler.SlotUsecase の偽物。呼ばれたメソッド名を覚える。DB には触れない。
type fakeSlots struct{ calls []string }

func (f *fakeSlots) Create(context.Context, usecase.Actor, usecase.CreateSlotInput) (entity.SlotWithRemaining, error) {
	f.calls = append(f.calls, "Create")
	return entity.SlotWithRemaining{}, errors.New("fake")
}

func (f *fakeSlots) Get(context.Context, usecase.Actor, string) (entity.SlotWithRemaining, error) {
	f.calls = append(f.calls, "Get")
	return entity.SlotWithRemaining{}, errors.New("fake")
}

func (f *fakeSlots) List(context.Context, usecase.Actor, usecase.ListSlotsInput) (usecase.SlotPage, error) {
	f.calls = append(f.calls, "List")
	return usecase.SlotPage{}, nil
}

func (f *fakeSlots) Update(context.Context, usecase.Actor, string, usecase.UpdateSlotInput) error {
	f.calls = append(f.calls, "Update")
	return errors.New("fake")
}

func (f *fakeSlots) Delete(context.Context, usecase.Actor, string) error {
	f.calls = append(f.calls, "Delete")
	return errors.New("fake")
}

func (f *fakeSlots) Publish(context.Context, usecase.Actor, string) error {
	f.calls = append(f.calls, "Publish")
	return errors.New("fake")
}

func (f *fakeSlots) Close(context.Context, usecase.Actor, string) error {
	f.calls = append(f.calls, "Close")
	return errors.New("fake")
}

// denyAll は、何も通さない偽の認証(全て 401)。Protected に届いた要求はここで止まる。
func denyAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

// allowAdmin は、管理者の Principal を context に入れて通す偽の認証。
func allowAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := middleware.Principal{Sub: "fake-sub", UserID: 1, IsAdmin: true}
		next.ServeHTTP(w, r.WithContext(middleware.ContextWithPrincipal(r.Context(), p)))
	})
}

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func mustBuildRoutes(t *testing.T, slots *fakeSlots, auth func(http.Handler) http.Handler) http.Handler {
	t.Helper()
	h, err := buildRoutes(routeDeps{slots: slots, auth: auth})
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	return h
}

func TestBuildRoutes_healthzPublicAnd405(t *testing.T) {
	h := mustBuildRoutes(t, &fakeSlots{}, denyAll) // 認証が全て 401 でも healthz は通る
	if rr := serve(h, http.MethodGet, "/healthz"); rr.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200(認証なしで通る)", rr.Code)
	}
	rr := serve(h, http.MethodPost, "/healthz")
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /healthz = %d Allow=%q, want 405 \"GET, HEAD\"", rr.Code, rr.Header().Get("Allow"))
	}
	if rr := serve(h, http.MethodGet, "/nope"); rr.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rr.Code)
	}
}

func TestBuildRoutes_protectedRoutesGoThroughAuth(t *testing.T) {
	slots := &fakeSlots{}
	h := mustBuildRoutes(t, slots, denyAll)
	id := entity.NewPublicID()
	for _, c := range []struct{ method, path string }{
		{"GET", "/slots"},
		{"POST", "/slots"},
		{"GET", "/slots/" + id},
		{"POST", "/slots/" + id + "/publish"},
		{"POST", "/slots/" + id + "/reservations"}, // 予約系も認証が先(501 より 401)
		{"GET", "/reservations"},
	} {
		if rr := serve(h, c.method, c.path); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401(認証ミドルウェアで止まる)", c.method, c.path, rr.Code)
		}
	}
	if len(slots.calls) != 0 {
		t.Errorf("認証を通らぬ要求が usecase に届いた: %v", slots.calls)
	}
}

func TestBuildRoutes_slotRoutesReachInjectedUsecase(t *testing.T) {
	slots := &fakeSlots{}
	h := mustBuildRoutes(t, slots, allowAdmin)
	id := entity.NewPublicID()
	cases := []struct{ method, path, want string }{
		{"GET", "/slots", "List"},
		{"GET", "/slots/" + id, "Get"},
		{"DELETE", "/slots/" + id, "Delete"},
		{"POST", "/slots/" + id + "/publish", "Publish"},
		{"POST", "/slots/" + id + "/close", "Close"},
	}
	for _, c := range cases {
		slots.calls = nil
		serve(h, c.method, c.path)
		if len(slots.calls) != 1 || slots.calls[0] != c.want {
			t.Errorf("%s %s: usecase の呼出 = %v, want [%s]", c.method, c.path, slots.calls, c.want)
		}
	}
}

func TestBuildRoutes_reservationRoutesAre501Problem(t *testing.T) {
	slots := &fakeSlots{}
	h := mustBuildRoutes(t, slots, allowAdmin)
	id := entity.NewPublicID()
	for _, c := range []struct{ method, path string }{
		{"POST", "/slots/" + id + "/reservations"},
		{"GET", "/slots/" + id + "/reservations"},
		{"GET", "/reservations"},
		{"GET", "/reservations/" + id},
		{"POST", "/reservations/" + id + "/cancel"},
	} {
		rr := serve(h, c.method, c.path)
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", c.method, c.path, rr.Code)
			continue
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s %s: Content-Type = %q", c.method, c.path, ct)
		}
		var p map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil || p["status"] != float64(501) {
			t.Errorf("%s %s: 本文が 501 の Problem でない: %s", c.method, c.path, rr.Body.String())
		}
	}
	if len(slots.calls) != 0 {
		t.Errorf("予約系の要求が枠の usecase に届いた: %v", slots.calls)
	}
}

func TestBuildRoutes_failsWithoutAuth(t *testing.T) {
	// Protected があるのに認証が無いまま起動してしまうのを、buildRoutes のエラーで止める。
	if _, err := buildRoutes(routeDeps{slots: &fakeSlots{}}); err == nil {
		t.Fatal("auth が nil なのに buildRoutes が成功した")
	}
}

var (
	reOpenAPIPath   = regexp.MustCompile(`^  (/\S*):\s*$`)
	reOpenAPIMethod = regexp.MustCompile(`^    (get|post|put|patch|delete|head|options):\s*$`)
	rePathParam     = regexp.MustCompile(`\{[^}]+\}`)
)

// openAPIOperations は、REPO の api/openapi.yaml の paths から (メソッド, パス) を全て読む。
// YAML ライブラリは使わず(go.mod を増やさない)、paths 直下の字下げの規則から正規表現で拾う。
func openAPIOperations(t *testing.T) []struct{ Method, Path string } {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("openapi.yaml を読めない: %v", err)
	}
	var ops []struct{ Method, Path string }
	inPaths, cur := false, ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if inPaths && line != "" && line[0] != ' ' && line[0] != '#' {
			break // paths の次のトップレベル(components など)
		}
		if !inPaths {
			continue
		}
		if m := reOpenAPIPath.FindStringSubmatch(line); m != nil {
			cur = m[1]
		} else if m := reOpenAPIMethod.FindStringSubmatch(line); m != nil && cur != "" {
			ops = append(ops, struct{ Method, Path string }{strings.ToUpper(m[1]), cur})
		}
	}
	return ops
}

func TestBuildRoutes_matchesOpenAPI(t *testing.T) {
	ops := openAPIOperations(t)
	// 読み取りが空・欠けのままで素通りしないよう、本数を固定する(healthz 1 + 枠 7 + 予約 5)。
	if len(ops) != 13 {
		t.Fatalf("openapi.yaml から読んだ操作が %d 本。13 本のはず: %v", len(ops), ops)
	}

	h := mustBuildRoutes(t, &fakeSlots{}, denyAll)
	id := entity.NewPublicID()
	concrete := func(path string) string { return rePathParam.ReplaceAllString(path, id) }

	methodsByPath := map[string]map[string]bool{}
	for _, op := range ops {
		if methodsByPath[op.Path] == nil {
			methodsByPath[op.Path] = map[string]bool{}
		}
		methodsByPath[op.Path][op.Method] = true

		// 登録されている: 404・405 にならない(認証なしゆえ healthz は 200、他は auth で 401)。
		rr := serve(h, op.Method, concrete(op.Path))
		if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s は openapi.yaml にあるが未登録(%d)", op.Method, op.Path, rr.Code)
		}
	}

	// 登録外: openapi.yaml に無いメソッドは 405。OpenAPI より多く登録してしまう誤りをここで拾う。
	for path, have := range methodsByPath {
		for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if have[m] {
				continue
			}
			if rr := serve(h, m, concrete(path)); rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s は openapi.yaml に無いのに %d(405 のはず)", m, path, rr.Code)
			}
		}
	}

	// 登録外: 知らぬパスは 404。
	for _, path := range []string{"/nope", "/slots/" + id + "/nope", "/reservations/" + id + "/nope", "/healthz/x"} {
		if rr := serve(h, "GET", path); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rr.Code)
		}
	}
}

// failingProvisioner は、必ず失敗する UserProvisioner。
type failingProvisioner struct{ err error }

func (f failingProvisioner) EnsureUser(context.Context, string) (int64, error) { return 0, f.err }

type okProvisioner struct{}

func (okProvisioner) EnsureUser(context.Context, string) (int64, error) { return 42, nil }

// callProvisioner は、Logging ミドルウェア(request_id つきの logger を context に入れる)の内側で EnsureUser を呼ぶ。
func callProvisioner(p middleware.UserProvisioner, logger *slog.Logger, sub string) (id int64, err error) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err = p.EnsureUser(r.Context(), sub)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", "req-prov-1")
	middleware.RequestID(middleware.Logging(logger)(inner)).ServeHTTP(httptest.NewRecorder(), req)
	return id, err
}

func TestLoggingProvisioner_logsKindAndReturnsOriginalError(t *testing.T) {
	const sub = "personal-sub-12345"
	cases := []struct {
		name     string
		err      error
		wantKind string
	}{
		{"MySQL のエラー", fmt.Errorf("利用者を確保できない: %w", &mysql.MySQLError{Number: 1213, Message: "Deadlock found for " + sub}), "mysql_error_1213"},
		{"タイムアウト", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "timeout"},
		{"sub が空", usecase.ErrEmptyKeycloakSub, "empty_sub"},
		{"想定外", errors.New("dial tcp 10.0.0.1:3306 for " + sub + " refused"), "other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, buf := testLogger()
			id, err := callProvisioner(loggingProvisioner{next: failingProvisioner{err: c.err}}, logger, sub)
			if !errors.Is(err, c.err) || id != 0 {
				t.Fatalf("元のエラーをそのまま返すはず: id=%d err=%v", id, err)
			}
			var found map[string]any
			for _, l := range logLines(t, buf) {
				if l["msg"] == "利用者の用意に失敗した" {
					found = l
				}
			}
			if found == nil {
				t.Fatalf("失敗の原因がログに出ていない: %s", buf.String())
			}
			if found["level"] != "ERROR" || found["cause"] != c.wantKind || found["request_id"] != "req-prov-1" {
				t.Errorf("ログの内容が違う(level・cause・request_id): %v", found)
			}
			if strings.Contains(buf.String(), sub) || strings.Contains(buf.String(), "dial tcp") {
				t.Errorf("エラー文(個人情報・内部事情)がログに漏れた: %s", buf.String())
			}
		})
	}
}

func TestLoggingProvisioner_successLogsNothingAndPassesID(t *testing.T) {
	logger, buf := testLogger()
	id, err := callProvisioner(loggingProvisioner{next: okProvisioner{}}, logger, "sub")
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v, want 42 nil", id, err)
	}
	for _, l := range logLines(t, buf) {
		if l["level"] == "ERROR" {
			t.Errorf("成功なのに ERROR ログ: %v", l)
		}
	}
}

func TestPingDB_unreachableWarnsWithoutDSN(t *testing.T) {
	logger, buf := testLogger()
	// 127.0.0.1:1 は誰も待ち受けておらず、すぐ拒否される。パスワードが DSN にあってもログに出ない。
	db, err := sql.Open("mysql", "app:ping-secret-pw@tcp(127.0.0.1:1)/reservation")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pingDB(context.Background(), db, logger)

	var warned bool
	for _, l := range logLines(t, buf) {
		if l["level"] == "WARN" && l["cause"] != nil {
			warned = true
		}
	}
	if !warned {
		t.Errorf("繋がらないのに警告ログが出ていない: %s", buf.String())
	}
	if strings.Contains(buf.String(), "ping-secret-pw") || strings.Contains(buf.String(), "127.0.0.1") {
		t.Errorf("DSN の中身がログに出た: %s", buf.String())
	}
}

func TestPingDB_canceledIsSilent(t *testing.T) {
	logger, buf := testLogger()
	db, err := sql.Open("mysql", "app:pw@tcp(127.0.0.1:1)/reservation")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pingDB(ctx, db, logger)

	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("停止中なのにログが出た: %s", buf.String())
	}
}
