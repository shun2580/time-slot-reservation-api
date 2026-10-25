package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var generatedID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// serve は、RequestID を通した結果として、レスポンスヘッダと context のIDを返す。
func serve(t *testing.T, header string, set bool) (headerID, ctxID string) {
	t.Helper()
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if set {
		req.Header.Set(RequestIDHeader, header)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Header().Get(RequestIDHeader), ctxID
}

func TestRequestID_generatesWhenAbsent(t *testing.T) {
	headerID, ctxID := serve(t, "", false)
	if !generatedID.MatchString(headerID) {
		t.Fatalf("生成したIDが16バイトの16進(32文字)でない: %q", headerID)
	}
	if ctxID != headerID {
		t.Fatalf("context のIDとレスポンスヘッダのIDが違う: ctx=%q header=%q", ctxID, headerID)
	}
}

func TestRequestID_generatedIDsDiffer(t *testing.T) {
	a, _ := serve(t, "", false)
	b, _ := serve(t, "", false)
	if a == b {
		t.Fatalf("2回の生成で同じIDになった: %q", a)
	}
}

func TestRequestID_adoptsValid(t *testing.T) {
	for _, id := range []string{
		"abc-123",
		"A",
		"-",
		"550e8400-e29b-41d4-a716-446655440000",
		strings.Repeat("a", 64), // 上限ちょうど
	} {
		headerID, ctxID := serve(t, id, true)
		if headerID != id || ctxID != id {
			t.Errorf("妥当なIDを採用しなかった: in=%q header=%q ctx=%q", id, headerID, ctxID)
		}
	}
}

func TestRequestID_replacesInvalid(t *testing.T) {
	for name, id := range map[string]string{
		"空":      "",
		"65文字":   strings.Repeat("a", 65),
		"空白入り":   "abc def",
		"下線":     "abc_def",
		"記号":     "abc;rm -rf",
		"日本語":    "あいう",
		"改行を含む様": "abc\tdef",
	} {
		headerID, ctxID := serve(t, id, true)
		if !generatedID.MatchString(headerID) {
			t.Errorf("%s: 不正なIDを置き換えなかった: in=%q header=%q", name, id, headerID)
		}
		if ctxID != headerID {
			t.Errorf("%s: context とヘッダが違う: ctx=%q header=%q", name, ctxID, headerID)
		}
	}
}

func TestRequestIDFrom_emptyWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := RequestIDFrom(req.Context()); got != "" {
		t.Fatalf("ミドルウェアを通していないのにIDが返った: %q", got)
	}
}
