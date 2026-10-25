package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// openapi.yaml 冒頭の表の行: `| /problems/xxx | 409 | 意味 |`
var problemRow = regexp.MustCompile(`(?m)^\s*\|\s*(/problems/[a-z0-9-]+)\s*\|\s*(\d{3})\s*\|`)

func TestBusinessProblemKinds_matchOpenAPITable(t *testing.T) {
	src, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("openapi.yaml を読めない: %v", err)
	}
	want := map[string]int{}
	for _, m := range problemRow.FindAllStringSubmatch(string(src), -1) {
		status, _ := strconv.Atoi(m[2])
		want[m[1]] = status
	}
	if len(want) != 11 {
		t.Fatalf("openapi.yaml の表から11件を抜けなかった: %d件 %v", len(want), want)
	}

	got := map[string]int{}
	for _, k := range BusinessProblemKinds() {
		if _, dup := got[k.Type]; dup {
			t.Errorf("type が重複: %s", k.Type)
		}
		got[k.Type] = k.Status
		if k.Title == "" {
			t.Errorf("title が空: %s", k.Type)
		}
	}
	if len(got) != len(want) {
		t.Errorf("件数が違う: コード%d件 / openapi%d件", len(got), len(want))
	}
	for typ, status := range want {
		gs, ok := got[typ]
		if !ok {
			t.Errorf("openapi にあるがコードに無い type: %s", typ)
			continue
		}
		if gs != status {
			t.Errorf("status が違う %s: コード%d / openapi%d", typ, gs, status)
		}
	}
	for typ := range got {
		if _, ok := want[typ]; !ok {
			t.Errorf("コードにあるが openapi に無い type: %s", typ)
		}
	}
}

func decodeProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("本文がJSONでない: %q: %v", rr.Body.String(), err)
	}
	return m
}

func TestWriteProblem_business(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/slots/abc/reservations?secret=1", nil)
	WriteProblem(rr, req, ProblemCapacityExceeded, "満席です")

	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	m := decodeProblem(t, rr)
	if m["type"] != "/problems/capacity-exceeded" || m["status"] != float64(409) || m["detail"] != "満席です" {
		t.Errorf("本文が違う: %v", m)
	}
	if m["title"] == "" || m["title"] == nil {
		t.Errorf("title が無い: %v", m)
	}
	if m["instance"] != "/slots/abc/reservations" {
		t.Errorf("instance はクエリを含まないパスのはず: %v", m["instance"])
	}
}

func TestWriteProblem_omitsEmptyDetail(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteProblem(rr, httptest.NewRequest(http.MethodGet, "/", nil), ProblemValidationError, "")
	m := decodeProblem(t, rr)
	if _, ok := m["detail"]; ok {
		t.Errorf("detail が空なら出さないはず: %v", m)
	}
	if _, ok := m["errors"]; ok {
		t.Errorf("errors は足さない: %v", m)
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestWriteStatusProblem_nonBusiness(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 501} {
		rr := httptest.NewRecorder()
		WriteStatusProblem(rr, httptest.NewRequest(http.MethodGet, "/x", nil), status, "")
		if rr.Code != status {
			t.Errorf("status = %d, want %d", rr.Code, status)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%d: Content-Type = %q", status, ct)
		}
		m := decodeProblem(t, rr)
		if m["type"] != "about:blank" || m["title"] != http.StatusText(status) || m["status"] != float64(status) {
			t.Errorf("%d: 本文が違う: %v", status, m)
		}
	}
}

func TestNotImplemented_is501Problem(t *testing.T) {
	rr := httptest.NewRecorder()
	NotImplemented(rr, httptest.NewRequest(http.MethodPost, "/slots/abc/reservations", nil))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	m := decodeProblem(t, rr)
	if m["status"] != float64(501) || m["title"] != "Not Implemented" {
		t.Errorf("本文が違う: %v", m)
	}
}

func TestInternalServerError_hidesInternals(t *testing.T) {
	rr := httptest.NewRecorder()
	InternalServerError(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	m := decodeProblem(t, rr)
	if _, ok := m["detail"]; ok {
		t.Errorf("500 の本文に detail を出してはならない: %v", m)
	}
}

func TestWriteValidationProblem_errors(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteValidationProblem(rr, httptest.NewRequest(http.MethodPost, "/slots", nil), []FieldError{
		{Field: "capacity", Message: "1以上にしてください"},
		{Field: "starts_at", Message: "必須です"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	m := decodeProblem(t, rr)
	if m["type"] != "/problems/validation-error" || m["status"] != float64(400) {
		t.Errorf("本文が違う: %v", m)
	}
	errs, ok := m["errors"].([]any)
	if !ok || len(errs) != 2 {
		t.Fatalf("errors が2件の配列のはず: %v", m["errors"])
	}
	first, _ := errs[0].(map[string]any)
	if first["field"] != "capacity" || first["message"] != "1以上にしてください" || len(first) != 2 {
		t.Errorf("errors[0] が違う: %v", first)
	}
}

func TestWriteValidationProblem_emptyErrorsOmitted(t *testing.T) {
	for name, errs := range map[string][]FieldError{"nil": nil, "empty": {}} {
		rr := httptest.NewRecorder()
		WriteValidationProblem(rr, httptest.NewRequest(http.MethodPost, "/slots", nil), errs)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rr.Code)
		}
		if m := decodeProblem(t, rr); m["errors"] != nil {
			t.Errorf("%s: errors キーを出してはならない: %v", name, m)
		}
		if strings.Contains(rr.Body.String(), `"errors"`) {
			t.Errorf("%s: 本文に errors が含まれる: %s", name, rr.Body.String())
		}
	}
}

func TestWriteStatusProblem_neverHasErrors(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteStatusProblem(rr, httptest.NewRequest(http.MethodGet, "/x", nil), http.StatusNotFound, "")
	if strings.Contains(rr.Body.String(), `"errors"`) {
		t.Errorf("validation-error 以外に errors を出してはならない: %s", rr.Body.String())
	}
}
