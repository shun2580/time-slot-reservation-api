package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/handler"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// 枠の7本の handler の試験。偽の SlotUsecase を差し、HTTP の入出力の変換だけを確かめる。
// 業務規則(何が 409 か 422 か)は usecase の試験、SQL は repository の試験の持分。ここでは
// 「usecase がこの error を返したら、この Problem Details になる」ことと、入力の「形」の検証・検証の順序を見る。

const slotTestID = "11111111-1111-4111-8111-111111111111"

// openapi.yaml の Slot の required(= 本文のキーの全部)。キーが増減したらこの試験が落ちる。
var slotKeys = []string{"id", "title", "capacity", "remaining", "startAt", "endAt", "reservationDeadlineAt", "status"}

// slotStub は handler.SlotUsecase の偽物。呼ばれた順と、受け取った引数を覚える。
type slotStub struct {
	err   error    // 全メソッドがこれを返す
	calls []string // 呼ばれたメソッド名(呼ばれなかったことの断言に使う)

	actor  usecase.Actor
	id     string
	create usecase.CreateSlotInput
	update usecase.UpdateSlotInput
	list   usecase.ListSlotsInput

	got  entity.SlotWithRemaining // Get が返す
	page usecase.SlotPage         // List が返す
}

// sampleSlot は UTC の時刻を持つ枠。handler が +09:00 にして出すことを確かめるため、わざと UTC で返す。
func sampleSlot() entity.SlotWithRemaining {
	return entity.SlotWithRemaining{
		Slot: entity.Slot{
			PublicID: slotTestID, Title: "朝の枠", Capacity: 10, Status: entity.SlotPublished,
			StartAt:               time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			EndAt:                 time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC),
			ReservationDeadlineAt: time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC),
		},
		Remaining: 4,
	}
}

func (s *slotStub) Create(_ context.Context, a usecase.Actor, in usecase.CreateSlotInput) (entity.SlotWithRemaining, error) {
	s.calls, s.actor, s.create = append(s.calls, "Create"), a, in
	if s.err != nil {
		return entity.SlotWithRemaining{}, s.err
	}
	return entity.SlotWithRemaining{
		Slot: entity.Slot{
			PublicID: slotTestID, Title: in.Title, Capacity: in.Capacity, Status: entity.SlotDraft,
			StartAt: in.StartAt, EndAt: in.EndAt, ReservationDeadlineAt: in.ReservationDeadlineAt,
		},
		Remaining: in.Capacity,
	}, nil
}

func (s *slotStub) Get(_ context.Context, a usecase.Actor, id string) (entity.SlotWithRemaining, error) {
	s.calls, s.actor, s.id = append(s.calls, "Get"), a, id
	if s.err != nil {
		return entity.SlotWithRemaining{}, s.err
	}
	return s.got, nil
}

func (s *slotStub) List(_ context.Context, a usecase.Actor, in usecase.ListSlotsInput) (usecase.SlotPage, error) {
	s.calls, s.actor, s.list = append(s.calls, "List"), a, in
	if s.err != nil {
		return usecase.SlotPage{}, s.err
	}
	return s.page, nil
}

func (s *slotStub) Update(_ context.Context, a usecase.Actor, id string, in usecase.UpdateSlotInput) error {
	s.calls, s.actor, s.id, s.update = append(s.calls, "Update"), a, id, in
	return s.err
}

func (s *slotStub) Delete(_ context.Context, a usecase.Actor, id string) error {
	s.calls, s.actor, s.id = append(s.calls, "Delete"), a, id
	return s.err
}

func (s *slotStub) Publish(_ context.Context, a usecase.Actor, id string) error {
	s.calls, s.actor, s.id = append(s.calls, "Publish"), a, id
	return s.err
}

func (s *slotStub) Close(_ context.Context, a usecase.Actor, id string) error {
	s.calls, s.actor, s.id = append(s.calls, "Close"), a, id
	return s.err
}

// 誰として呼ぶか。
type caller int

const (
	asUser   caller = iota // 認証済みの利用者
	asAdmin                // 認証済みの管理者
	asNobody               // Principal が context に無い(配線の誤りの保険を見る)
)

// slotEnv は、Router + 偽の認証 + RequestID + Logging + SlotHandler を組み、リクエストを1本流す。
type slotEnv struct {
	uc   *slotStub
	logs bytes.Buffer
}

func newSlotEnv(uc *slotStub) *slotEnv { return &slotEnv{uc: uc} }

func (e *slotEnv) do(t *testing.T, as caller, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := handler.NewRouter()
	r.SetAuth(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if as != asNobody {
				p := middleware.Principal{Sub: "test-sub", UserID: 7, IsAdmin: as == asAdmin}
				req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), p))
			}
			next.ServeHTTP(w, req)
		})
	})
	handler.RegisterSlotRoutes(r, handler.NewSlotHandler(e.uc))
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(&e.logs, nil))
	h = middleware.RequestID(middleware.Logging(logger)(h))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

// ---- 断言の道具 ----

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("本文が JSON のオブジェクトでない: %v\n%s", err, rec.Body.String())
	}
	return m
}

func assertKeys(t *testing.T, m map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	w := append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(w)
	if !reflect.DeepEqual(got, w) {
		t.Errorf("キーが違う\n got: %v\nwant: %v", got, w)
	}
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Errorf("status = %d, want %d\n%s", rec.Code, want, rec.Body.String())
	}
}

// assertProblem は応答が Problem Details で、status と type が合うことを確かめ、本文を返す。
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) map[string]any {
	t.Helper()
	assertStatus(t, rec, status)
	if ct := rec.Header().Get("Content-Type"); ct != handler.ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, handler.ContentTypeProblem)
	}
	m := decodeMap(t, rec)
	if m["type"] != typ {
		t.Errorf("type = %v, want %s", m["type"], typ)
	}
	if got, _ := m["status"].(float64); int(got) != status {
		t.Errorf("本文の status = %v, want %d", m["status"], status)
	}
	return m
}

// problemFields は validation-error の errors[] から項目名を順に取り出す。
func problemFields(t *testing.T, m map[string]any) []string {
	t.Helper()
	errs, ok := m["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors[] が無い: %v", m)
	}
	var fields []string
	for _, e := range errs {
		em := e.(map[string]any)
		if em["message"] == "" || em["message"] == nil {
			t.Errorf("message が空: %v", em)
		}
		fields = append(fields, em["field"].(string))
	}
	return fields
}

func assertNoCalls(t *testing.T, uc *slotStub) {
	t.Helper()
	if len(uc.calls) != 0 {
		t.Errorf("usecase が呼ばれてはならない: %v", uc.calls)
	}
}

const validCreate = `{"title":"朝の枠","capacity":3,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`

// 7本の一覧。表駆動の試験で使う。管理者専用かどうか、本文を取るかどうかも持つ。
var slotEndpoints = []struct {
	name, method, path, body string
	adminOnly                bool
}{
	{"list", "GET", "/slots", "", false},
	{"create", "POST", "/slots", validCreate, true},
	{"get", "GET", "/slots/" + slotTestID, "", false},
	{"update", "PATCH", "/slots/" + slotTestID, `{"capacity":5}`, true},
	{"delete", "DELETE", "/slots/" + slotTestID, "", true},
	{"publish", "POST", "/slots/" + slotTestID + "/publish", "", true},
	{"close", "POST", "/slots/" + slotTestID + "/close", "", true},
}

// ---- 成功: 7本の status と本文の形 ----

func TestSlotCreate_Success(t *testing.T) {
	uc := &slotStub{}
	rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", validCreate)

	assertStatus(t, rec, 201)
	if got := rec.Header().Get("Location"); got != "/slots/"+slotTestID {
		t.Errorf("Location = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	m := decodeMap(t, rec)
	assertKeys(t, m, slotKeys)
	if m["id"] != slotTestID || m["title"] != "朝の枠" || m["status"] != "draft" || m["capacity"] != float64(3) || m["remaining"] != float64(3) {
		t.Errorf("本文が違う: %v", m)
	}
	// usecase が受け取った値: 項目が正しく写っている・Actor は Principal から作られている。
	want := usecase.CreateSlotInput{
		Title: "朝の枠", Capacity: 3,
		StartAt:               time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		EndAt:                 time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC),
		ReservationDeadlineAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
	}
	if g := uc.create; g.Title != want.Title || g.Capacity != want.Capacity || !g.StartAt.Equal(want.StartAt) || !g.EndAt.Equal(want.EndAt) || !g.ReservationDeadlineAt.Equal(want.ReservationDeadlineAt) {
		t.Errorf("usecase に渡った入力が違う: %+v", g)
	}
	if uc.actor != (usecase.Actor{UserID: 7, Sub: "test-sub", IsAdmin: true}) {
		t.Errorf("Actor = %+v", uc.actor)
	}
}

func TestSlotGet_Success(t *testing.T) {
	uc := &slotStub{got: sampleSlot()}
	rec := newSlotEnv(uc).do(t, asUser, "GET", "/slots/"+slotTestID, "")

	assertStatus(t, rec, 200)
	m := decodeMap(t, rec)
	assertKeys(t, m, slotKeys)
	if m["remaining"] != float64(4) || m["capacity"] != float64(10) || m["status"] != "published" {
		t.Errorf("本文が違う: %v", m)
	}
	if uc.id != slotTestID || uc.actor.IsAdmin || uc.actor.UserID != 7 {
		t.Errorf("usecase への引数が違う: id=%q actor=%+v", uc.id, uc.actor)
	}
}

func TestSlotList_Success(t *testing.T) {
	uc := &slotStub{page: usecase.SlotPage{Items: []entity.SlotWithRemaining{sampleSlot(), sampleSlot()}, Limit: 20, Offset: 40}}
	rec := newSlotEnv(uc).do(t, asUser, "GET", "/slots", "")

	assertStatus(t, rec, 200)
	m := decodeMap(t, rec)
	assertKeys(t, m, []string{"items", "limit", "offset"})
	if m["limit"] != float64(20) || m["offset"] != float64(40) { // usecase が返した実効値をそのまま出す
		t.Errorf("limit/offset = %v/%v", m["limit"], m["offset"])
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d 件", len(items))
	}
	for _, it := range items {
		assertKeys(t, it.(map[string]any), slotKeys)
	}
}

// 0件でも items は [](null にしない)。nil のスライスと空のスライスのどちらが来ても。
func TestSlotList_EmptyItemsIsArrayNotNull(t *testing.T) {
	for name, items := range map[string][]entity.SlotWithRemaining{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			uc := &slotStub{page: usecase.SlotPage{Items: items, Limit: 20}}
			rec := newSlotEnv(uc).do(t, asUser, "GET", "/slots", "")
			assertStatus(t, rec, 200)
			if !strings.Contains(rec.Body.String(), `"items":[]`) {
				t.Errorf("items は [] でなければならない: %s", rec.Body.String())
			}
		})
	}
}

// PATCH・DELETE・publish・close は 204 で本文なし。usecase へ ID と Actor が渡る。
func TestSlotCommands_NoContent(t *testing.T) {
	cases := []struct{ name, method, path, body, call string }{
		{"update", "PATCH", "/slots/" + slotTestID, `{"capacity":5}`, "Update"},
		{"delete", "DELETE", "/slots/" + slotTestID, "", "Delete"},
		{"publish", "POST", "/slots/" + slotTestID + "/publish", "", "Publish"},
		{"close", "POST", "/slots/" + slotTestID + "/close", "", "Close"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asAdmin, c.method, c.path, c.body)
			assertStatus(t, rec, 204)
			if rec.Body.Len() != 0 {
				t.Errorf("204 に本文がある: %q", rec.Body.String())
			}
			if !reflect.DeepEqual(uc.calls, []string{c.call}) || uc.id != slotTestID {
				t.Errorf("calls=%v id=%q", uc.calls, uc.id)
			}
			if uc.actor != (usecase.Actor{UserID: 7, Sub: "test-sub", IsAdmin: true}) {
				t.Errorf("Actor = %+v", uc.actor)
			}
		})
	}
}

// ---- 日時: 入力は RFC 3339 の任意のオフセット、出力は +09:00 ----

func TestSlotTimes_AcceptAnyOffsetAndOutputJST(t *testing.T) {
	// 同じ瞬間を Z・+00:00・-05:00・+09:00 で送る。どれも受理され、出力は +09:00 になる。
	for _, tc := range []struct{ name, start, end, deadline string }{
		{"Z", "2026-11-01T00:00:00Z", "2026-11-01T01:00:00Z", "2026-10-31T15:00:00Z"},
		{"+00:00", "2026-11-01T00:00:00+00:00", "2026-11-01T01:00:00+00:00", "2026-10-31T15:00:00+00:00"},
		{"-05:00", "2026-10-31T19:00:00-05:00", "2026-10-31T20:00:00-05:00", "2026-10-31T10:00:00-05:00"},
		{"+09:00", "2026-11-01T09:00:00+09:00", "2026-11-01T10:00:00+09:00", "2026-11-01T00:00:00+09:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"title":"a","capacity":3,"startAt":%q,"endAt":%q,"reservationDeadlineAt":%q}`, tc.start, tc.end, tc.deadline)
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", body)
			assertStatus(t, rec, 201)
			m := decodeMap(t, rec)
			if m["startAt"] != "2026-11-01T09:00:00+09:00" || m["endAt"] != "2026-11-01T10:00:00+09:00" || m["reservationDeadlineAt"] != "2026-11-01T00:00:00+09:00" {
				t.Errorf("出力が +09:00 でない: %v %v %v", m["startAt"], m["endAt"], m["reservationDeadlineAt"])
			}
			if !uc.create.StartAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("瞬間が変わった: %v", uc.create.StartAt)
			}
		})
	}
}

// Get・List が返す UTC の時刻も +09:00 で出る(出力の変換は DTO の1か所)。
func TestSlotTimes_ResponseConvertsToJST(t *testing.T) {
	uc := &slotStub{got: sampleSlot(), page: usecase.SlotPage{Items: []entity.SlotWithRemaining{sampleSlot()}, Limit: 20}}
	m := decodeMap(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots/"+slotTestID, ""))
	if m["startAt"] != "2026-11-01T09:00:00+09:00" {
		t.Errorf("Get の startAt = %v", m["startAt"])
	}
	l := decodeMap(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots", ""))
	first := l["items"].([]any)[0].(map[string]any)
	if first["reservationDeadlineAt"] != "2026-11-01T00:00:00+09:00" {
		t.Errorf("List の reservationDeadlineAt = %v", first["reservationDeadlineAt"])
	}
}

// ---- 入力の形の誤り(POST /slots)→ 400。usecase は呼ばれない ----

func TestSlotCreate_BadShape(t *testing.T) {
	cases := []struct {
		name, body string
		fields     []string // errors[] の項目名(空なら件数・名前は問わず errors[] があることだけ)
	}{
		{"必須が全部欠ける", `{}`, []string{"title", "capacity", "startAt", "endAt", "reservationDeadlineAt"}},
		{"一部だけ欠ける", `{"title":"a"}`, []string{"capacity", "startAt", "endAt", "reservationDeadlineAt"}},
		{"null は無いのと同じ", `{"title":null,"capacity":3,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"title"}},
		{"未知の項目", `{"title":"a","capacity":3,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00","status":"closed"}`, nil},
		{"capacity が文字列", `{"title":"a","capacity":"3","startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"capacity"}},
		{"capacity が小数", `{"title":"a","capacity":3.5,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"capacity"}},
		{"title が数値", `{"title":123,"capacity":3,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"title"}},
		{"オフセット無しの startAt", `{"title":"a","capacity":3,"startAt":"2026-11-01T09:00:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"startAt"}},
		{"日付だけの endAt", `{"title":"a","capacity":3,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"endAt"}},
		{"日時が数値", `{"title":"a","capacity":3,"startAt":1793494800,"endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`, []string{"startAt"}},
		{"日時が3つとも誤り", `{"title":"a","capacity":3,"startAt":"x","endAt":"y","reservationDeadlineAt":"z"}`, []string{"startAt", "endAt", "reservationDeadlineAt"}},
		{"壊れた JSON", `{garbage`, nil},
		{"空の本文", ``, nil},
		{"オブジェクトでない(配列)", `[]`, nil},
		{"オブジェクトでない(文字列)", `"a"`, nil},
		{"後ろに余計な値", validCreate + ` {"x":1}`, nil},
		{"後ろに余計な文字", validCreate + ` x`, nil},
		{"後ろに余計な閉じ括弧(json.Decoder.More の見逃し)", validCreate + ` }`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", c.body)
			m := assertProblem(t, rec, 400, "/problems/validation-error")
			fields := problemFields(t, m)
			if c.fields != nil && !reflect.DeepEqual(fields, c.fields) {
				t.Errorf("errors[] の項目 = %v, want %v", fields, c.fields)
			}
			assertNoCalls(t, uc)
		})
	}
}

// 本文が大きすぎる(1 MiB 超)→ 400。上限を超えても usecase に届かない。
func TestSlotCreate_OversizedBody(t *testing.T) {
	big := `{"title":"` + strings.Repeat("a", (1<<20)+1) + `"}`
	uc := &slotStub{}
	rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", big)
	assertProblem(t, rec, 400, "/problems/validation-error")
	assertNoCalls(t, uc)

	// PATCH も同じ上限。
	rec = newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, `{"title":"`+strings.Repeat("a", (1<<20)+1)+`"}`)
	assertProblem(t, rec, 400, "/problems/validation-error")
	assertNoCalls(t, uc)
}

// ちょうど上限以下なら読める(上限の境界が 1 MiB であること)。title が長すぎる検査は usecase の持分。
func TestSlotCreate_BodyAtLimitIsRead(t *testing.T) {
	pad := (1 << 20) - len(`{"title":"","capacity":1,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`)
	body := `{"title":"` + strings.Repeat("a", pad) + `","capacity":1,"startAt":"2026-11-01T09:00:00+09:00","endAt":"2026-11-01T10:00:00+09:00","reservationDeadlineAt":"2026-10-31T09:00:00+09:00"}`
	if len(body) != 1<<20 {
		t.Fatalf("試験の組み立てが違う: %d", len(body))
	}
	uc := &slotStub{}
	rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", body)
	assertStatus(t, rec, 201)
}

// ---- PATCH: 「送った項目」の判別 ----

func TestSlotUpdate_PresenceOfFields(t *testing.T) {
	t.Run("capacity だけ", func(t *testing.T) {
		uc := &slotStub{}
		assertStatus(t, newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, `{"capacity":5}`), 204)
		u := uc.update
		if u.Capacity == nil || *u.Capacity != 5 || u.Title != nil || u.StartAt != nil || u.EndAt != nil || u.ReservationDeadlineAt != nil {
			t.Errorf("送った項目だけが入るはず: %+v", u)
		}
	})
	t.Run("全項目", func(t *testing.T) {
		uc := &slotStub{}
		body := `{"title":"新","capacity":9,"startAt":"2026-11-01T00:00:00Z","endAt":"2026-11-01T01:00:00Z","reservationDeadlineAt":"2026-10-31T15:00:00+00:00"}`
		assertStatus(t, newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, body), 204)
		u := uc.update
		if u.Title == nil || *u.Title != "新" || u.Capacity == nil || *u.Capacity != 9 ||
			u.StartAt == nil || !u.StartAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
			u.EndAt == nil || !u.EndAt.Equal(time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)) ||
			u.ReservationDeadlineAt == nil || !u.ReservationDeadlineAt.Equal(time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)) {
			t.Errorf("全項目が入るはず: %+v", u)
		}
	})
	t.Run("ゼロ値も『送った』とみなす", func(t *testing.T) {
		// capacity:0 や title:"" は「無い」ではなく「0・空を送った」。検査(400)は usecase が行う。
		uc := &slotStub{}
		assertStatus(t, newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, `{"capacity":0,"title":""}`), 204)
		if uc.update.Capacity == nil || *uc.update.Capacity != 0 || uc.update.Title == nil || *uc.update.Title != "" {
			t.Errorf("ゼロ値が落ちた: %+v", uc.update)
		}
	})
	t.Run("空のオブジェクトは handler は通す(usecase が 400 にする)", func(t *testing.T) {
		uc := &slotStub{}
		assertStatus(t, newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, `{}`), 204)
		if !reflect.DeepEqual(uc.update, usecase.UpdateSlotInput{}) || !reflect.DeepEqual(uc.calls, []string{"Update"}) {
			t.Errorf("usecase に空の入力が届くはず: %+v %v", uc.update, uc.calls)
		}
	})
}

func TestSlotUpdate_BadShape(t *testing.T) {
	cases := []struct {
		name, body string
		fields     []string
	}{
		{"null", `{"title":null}`, []string{"title"}},
		{"全部 null", `{"title":null,"capacity":null}`, []string{"capacity", "title"}},
		{"許可項目以外 status", `{"status":"closed"}`, []string{"status"}},
		{"許可項目以外 id", `{"id":"x","capacity":5}`, []string{"id"}},
		{"未知の項目は項目名順に並ぶ", `{"zzz":1,"aaa":2}`, []string{"aaa", "zzz"}},
		{"capacity が文字列", `{"capacity":"x"}`, []string{"capacity"}},
		{"capacity が小数", `{"capacity":5.5}`, []string{"capacity"}},
		{"title が数値", `{"title":1}`, []string{"title"}},
		{"オフセット無しの startAt", `{"startAt":"2026-11-01T09:00:00"}`, []string{"startAt"}},
		{"日時が数値", `{"endAt":123}`, []string{"endAt"}},
		{"日時の形の誤りと null の混在", `{"reservationDeadlineAt":"x","endAt":null}`, []string{"endAt", "reservationDeadlineAt"}},
		{"配列", `[]`, nil},
		{"文字列", `"a"`, nil},
		{"JSON の null", `null`, nil},
		{"壊れた JSON", `{garbage`, nil},
		{"空の本文", ``, nil},
		{"後ろに余計な値", `{"capacity":5} {"x":1}`, nil},
		{"後ろに余計な閉じ括弧", `{"capacity":5} }`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/"+slotTestID, c.body)
			m := assertProblem(t, rec, 400, "/problems/validation-error")
			fields := problemFields(t, m)
			if c.fields != nil && !reflect.DeepEqual(fields, c.fields) {
				t.Errorf("errors[] の項目 = %v, want %v", fields, c.fields)
			}
			assertNoCalls(t, uc)
		})
	}
}

// ---- 一覧のクエリ: 形の誤りだけ handler が 400。範囲(limit 1〜100 など)は usecase ----

func TestSlotList_QueryIsPassedToUsecase(t *testing.T) {
	uc := &slotStub{}
	q := "from=2026-11-01T00:00:00%2B09:00&to=2026-11-02T00:00:00Z&limit=50&offset=10&availableOnly=true&status=draft"
	assertStatus(t, newSlotEnv(uc).do(t, asAdmin, "GET", "/slots?"+q, ""), 200)
	in := uc.list
	if in.From == nil || !in.From.Equal(time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("From = %v", in.From)
	}
	if in.To == nil || !in.To.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("To = %v", in.To)
	}
	if in.Limit == nil || *in.Limit != 50 || in.Offset == nil || *in.Offset != 10 || !in.AvailableOnly {
		t.Errorf("Limit/Offset/AvailableOnly = %v/%v/%v", in.Limit, in.Offset, in.AvailableOnly)
	}
	if in.Status == nil || *in.Status != entity.SlotDraft {
		t.Errorf("Status = %v", in.Status)
	}
}

func TestSlotList_NoQueryMeansUnspecified(t *testing.T) {
	uc := &slotStub{}
	assertStatus(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots", ""), 200)
	if !reflect.DeepEqual(uc.list, usecase.ListSlotsInput{}) {
		t.Errorf("何も指定しなければ全て『指定なし』: %+v", uc.list)
	}
	// availableOnly=false は『指定なし』と同じ。空の値も指定なし。
	uc = &slotStub{}
	assertStatus(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots?availableOnly=false&limit=&status=", ""), 200)
	if !reflect.DeepEqual(uc.list, usecase.ListSlotsInput{}) {
		t.Errorf("false・空の値は指定なし: %+v", uc.list)
	}
}

// 範囲の検査は usecase の持分。handler は「整数として読めた値」をそのまま渡す。status の値も検査しない。
func TestSlotList_RangeAndStatusAreNotJudgedByHandler(t *testing.T) {
	uc := &slotStub{}
	assertStatus(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots?limit=0&offset=-1&status=bogus", ""), 200)
	if uc.list.Limit == nil || *uc.list.Limit != 0 || uc.list.Offset == nil || *uc.list.Offset != -1 {
		t.Errorf("Limit/Offset がそのまま渡るはず: %v/%v", uc.list.Limit, uc.list.Offset)
	}
	if uc.list.Status == nil || *uc.list.Status != "bogus" {
		t.Errorf("status は検査せず渡すはず: %v", uc.list.Status)
	}
	// 利用者の status も handler は落とさない(無視するのは usecase の役目)。
	if uc.actor.IsAdmin {
		t.Error("利用者として呼んだ")
	}
}

func TestSlotList_BadQueryShape(t *testing.T) {
	cases := []struct {
		query  string
		fields []string
	}{
		{"limit=abc", []string{"limit"}},
		{"limit=1.5", []string{"limit"}},
		{"offset=abc", []string{"offset"}},
		{"availableOnly=yes", []string{"availableOnly"}},
		{"availableOnly=1", []string{"availableOnly"}},
		{"availableOnly=TRUE", []string{"availableOnly"}},
		{"from=2026-11-01", []string{"from"}},
		{"from=2026-11-01T00:00:00", []string{"from"}}, // オフセット無し
		{"to=bad", []string{"to"}},
		{"from=x&to=y&limit=z&offset=w&availableOnly=q", []string{"from", "to", "limit", "offset", "availableOnly"}},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			for _, as := range []caller{asUser, asAdmin} {
				uc := &slotStub{}
				rec := newSlotEnv(uc).do(t, as, "GET", "/slots?"+c.query, "")
				m := assertProblem(t, rec, 400, "/problems/validation-error")
				if got := problemFields(t, m); !reflect.DeepEqual(got, c.fields) {
					t.Errorf("errors[] の項目 = %v, want %v", got, c.fields)
				}
				assertNoCalls(t, uc)
			}
		})
	}
}

// ---- {id} の形: UUID でなければ 404(usecase に届かない) ----

func TestSlotID_NotUUIDIs404(t *testing.T) {
	bad := []string{"not-a-uuid", "123", slotTestID + "0", slotTestID[:35], "{" + slotTestID + "}", strings.ReplaceAll(slotTestID, "-", ""), "gggggggg-1111-4111-8111-111111111111"}
	routes := []struct{ name, method, suffix, body string }{
		{"get", "GET", "", ""},
		{"update", "PATCH", "", `{"capacity":5}`},
		{"delete", "DELETE", "", ""},
		{"publish", "POST", "/publish", ""},
		{"close", "POST", "/close", ""},
	}
	for _, id := range bad {
		for _, rt := range routes {
			t.Run(rt.name+"/"+id, func(t *testing.T) {
				uc := &slotStub{}
				rec := newSlotEnv(uc).do(t, asAdmin, rt.method, "/slots/"+id+rt.suffix, rt.body)
				assertProblem(t, rec, 404, "about:blank")
				assertNoCalls(t, uc)
			})
		}
	}
}

// 大文字の UUID は受理し、小文字にそろえて usecase へ渡す。
func TestSlotID_UppercaseIsNormalized(t *testing.T) {
	uc := &slotStub{got: sampleSlot()}
	assertStatus(t, newSlotEnv(uc).do(t, asUser, "GET", "/slots/"+strings.ToUpper(slotTestID), ""), 200)
	if uc.id != slotTestID {
		t.Errorf("usecase に渡る ID = %q", uc.id)
	}
}

// ---- 検証の順序: 401 → 403 → 400 → 404 → usecase ----

// 非管理者は、本文が壊れていても 403(400 より先)。{id} が不正でも 403(404 より先)。usecase は呼ばれない。
func TestSlotOrder_ForbiddenBeforeBadBodyAndBadID(t *testing.T) {
	cases := []struct{ name, method, path, body string }{
		{"create+壊れた本文", "POST", "/slots", `{garbage`},
		{"create+空の本文", "POST", "/slots", ``},
		{"create+必須欠け", "POST", "/slots", `{}`},
		{"update+壊れた本文", "PATCH", "/slots/" + slotTestID, `{garbage`},
		{"update+null", "PATCH", "/slots/" + slotTestID, `{"title":null}`},
		{"update+不正な ID+壊れた本文", "PATCH", "/slots/not-a-uuid", `{garbage`},
		{"delete+不正な ID", "DELETE", "/slots/not-a-uuid", ""},
		{"publish+不正な ID", "POST", "/slots/not-a-uuid/publish", ""},
		{"close+不正な ID", "POST", "/slots/not-a-uuid/close", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asUser, c.method, c.path, c.body)
			assertProblem(t, rec, 403, "about:blank")
			assertNoCalls(t, uc)
		})
	}
}

// 管理者で本文が壊れていて ID も不正なら 400(404 より先)。
func TestSlotOrder_BadBodyBeforeBadID(t *testing.T) {
	uc := &slotStub{}
	rec := newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/not-a-uuid", `{garbage`)
	assertProblem(t, rec, 400, "/problems/validation-error")
	assertNoCalls(t, uc)

	rec = newSlotEnv(uc).do(t, asAdmin, "PATCH", "/slots/not-a-uuid", `{"capacity":5}`)
	assertProblem(t, rec, 404, "about:blank")
	assertNoCalls(t, uc)
}

// 管理者専用の判定は handler では usecase.RequireAdmin の1か所。5本の管理者専用で非管理者は 403、
// 利用者も使える Get・List は通る。
func TestSlotAuthorization_AdminOnlyEndpoints(t *testing.T) {
	for _, ep := range slotEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			uc := &slotStub{got: sampleSlot()}
			rec := newSlotEnv(uc).do(t, asUser, ep.method, ep.path, ep.body)
			if ep.adminOnly {
				assertProblem(t, rec, 403, "about:blank")
				assertNoCalls(t, uc)
			} else {
				assertStatus(t, rec, 200)
				if len(uc.calls) != 1 {
					t.Errorf("利用者でも usecase まで届くはず: %v", uc.calls)
				}
			}
		})
	}
}

// Principal が無い(認証ミドルウェアを通っていない配線の誤り)→ 401。usecase は呼ばれない。
func TestSlot_NoPrincipalIs401(t *testing.T) {
	for _, ep := range slotEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			uc := &slotStub{}
			rec := newSlotEnv(uc).do(t, asNobody, ep.method, ep.path, ep.body)
			assertProblem(t, rec, 401, "about:blank")
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			assertNoCalls(t, uc)
		})
	}
}

// 認証が通っていれば、正常系・業務エラーのどちらでも handler は WWW-Authenticate を出さない(401 は middleware の仕事)。
func TestSlot_NoWWWAuthenticateWhenAuthenticated(t *testing.T) {
	for _, err := range []error{nil, usecase.ErrForbidden, usecase.ErrNotFound, usecase.Business(usecase.CodeInvalidStateTransition, "d")} {
		uc := &slotStub{err: err}
		rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots/"+slotTestID+"/close", "")
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("err=%v: WWW-Authenticate = %q", err, got)
		}
	}
}

// ---- usecase のエラー → Problem Details ----

func TestSlot_UsecaseErrorsMapToProblems(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		typ    string
	}{
		{"権限なし", usecase.ErrForbidden, 403, "about:blank"},
		{"無い", usecase.ErrNotFound, 404, "about:blank"},
		{"409 有効な予約がある枠を閉じる", usecase.Business(usecase.CodeSlotHasActiveReservations, "d"), 409, "/problems/slot-has-active-reservations"},
		{"422 予約履歴のある枠の削除", usecase.Business(usecase.CodeSlotHasReservationHistory, "d"), 422, "/problems/slot-has-reservation-history"},
		{"422 状態遷移", usecase.Business(usecase.CodeInvalidStateTransition, "d"), 422, "/problems/invalid-state-transition"},
		{"422 変更できない項目", usecase.Business(usecase.CodeUpdateNotAllowed, "d"), 422, "/problems/update-not-allowed"},
		// errors.As/Is で取り出す: fmt.Errorf("%w") で包まれていても写る。
		{"包まれた業務エラー", fmt.Errorf("close: %w", usecase.Business(usecase.CodeSlotHasActiveReservations, "d")), 409, "/problems/slot-has-active-reservations"},
		{"包まれた権限なし", fmt.Errorf("x: %w", usecase.ErrForbidden), 403, "about:blank"},
		{"包まれた無い", fmt.Errorf("x: %w", usecase.ErrNotFound), 404, "about:blank"},
	}
	// 7本のどれが返しても同じ写像になる(写像は writeError の1か所)。
	for _, ep := range slotEndpoints {
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				uc := &slotStub{err: c.err}
				rec := newSlotEnv(uc).do(t, asAdmin, ep.method, ep.path, ep.body)
				assertProblem(t, rec, c.status, c.typ)
				if len(uc.calls) != 1 {
					t.Errorf("usecase が1回呼ばれるはず: %v", uc.calls)
				}
			})
		}
	}
}

// 業務エラーの detail は本文に出る(利用者に見せてよい文言だけが usecase から来る約束)。
func TestSlot_BusinessDetailIsShown(t *testing.T) {
	uc := &slotStub{err: usecase.Business(usecase.CodeSlotHasActiveReservations, "有効な予約がある枠は閉じられない")}
	m := assertProblem(t, newSlotEnv(uc).do(t, asAdmin, "POST", "/slots/"+slotTestID+"/close", ""), 409, "/problems/slot-has-active-reservations")
	if m["detail"] != "有効な予約がある枠は閉じられない" || m["instance"] != "/slots/"+slotTestID+"/close" {
		t.Errorf("detail/instance = %v / %v", m["detail"], m["instance"])
	}
}

// usecase の ValidationError → 400 + errors[](項目名と message が写る)。
func TestSlot_ValidationErrorMapsToErrors(t *testing.T) {
	uc := &slotStub{err: &usecase.ValidationError{Fields: []usecase.FieldError{
		{Field: "capacity", Message: "1〜1000で指定してください"},
		{Field: "startAt", Message: "endAt より前にしてください"},
	}}}
	m := assertProblem(t, newSlotEnv(uc).do(t, asAdmin, "POST", "/slots", validCreate), 400, "/problems/validation-error")
	errs := m["errors"].([]any)
	if len(errs) != 2 {
		t.Fatalf("errors[] = %v", errs)
	}
	first := errs[0].(map[string]any)
	if first["field"] != "capacity" || first["message"] != "1〜1000で指定してください" {
		t.Errorf("errors[0] = %v", first)
	}
}

// 想定外の error(DB の障害など)→ 500。本文に内部の事情を出さず、request_id つきでログに残す。
func TestSlot_UnexpectedErrorIs500WithoutLeak(t *testing.T) {
	secret := "dial tcp 10.0.0.5:3306: password=hunter2 refused"
	for _, ep := range slotEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			uc := &slotStub{err: errors.New(secret)}
			env := newSlotEnv(uc)
			rec := env.do(t, asAdmin, ep.method, ep.path, ep.body)
			m := assertProblem(t, rec, 500, "about:blank")
			if m["detail"] != nil {
				t.Errorf("500 に detail を出してはならない: %v", m["detail"])
			}
			for _, leak := range []string{"dial", "10.0.0.5", "hunter2", "refused"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("本文に内部の事情 %q が漏れている: %s", leak, rec.Body.String())
				}
			}
			logs := env.logs.String()
			if !strings.Contains(logs, "hunter2") || !strings.Contains(logs, "request_id=") || strings.Contains(logs, "request_id= ") {
				t.Errorf("ログに原因と request_id が残るはず: %s", logs)
			}
		})
	}
}

// ---- 写像表: 10種の業務エラーが漏れなく 409/422 に写る ----

func TestSlot_AllBusinessCodesMapped(t *testing.T) {
	codes := usecase.AllBusinessCodes()
	if len(codes) != 10 {
		t.Fatalf("業務エラーの Code は 10 種のはず: %d", len(codes))
	}
	known := map[string]handler.ProblemKind{}
	for _, k := range handler.BusinessProblemKinds() {
		known[k.Type] = k
	}
	for _, c := range codes {
		t.Run(string(c), func(t *testing.T) {
			uc := &slotStub{err: usecase.Business(c, "d")}
			rec := newSlotEnv(uc).do(t, asAdmin, "POST", "/slots/"+slotTestID+"/close", "")
			typ := "/problems/" + string(c)
			k, ok := known[typ]
			if !ok {
				t.Fatalf("problem.go の種類表に %s が無い", typ)
			}
			if k.Status != 409 && k.Status != 422 {
				t.Fatalf("業務エラーは 409/422 のはず: %d", k.Status)
			}
			// 500(写像漏れ)ではなく、種類表と同じ type・status になる。
			assertProblem(t, rec, k.Status, k.Type)
		})
	}
}

// 写像表に無い Code(定義漏れ)は 409/422 に偽装せず 500。ログに Code が残る。
func TestSlot_UnknownBusinessCodeIs500(t *testing.T) {
	uc := &slotStub{err: usecase.Business(usecase.Code("no-such-code"), "d")}
	env := newSlotEnv(uc)
	rec := env.do(t, asAdmin, "POST", "/slots/"+slotTestID+"/close", "")
	assertProblem(t, rec, 500, "about:blank")
	if !strings.Contains(env.logs.String(), "no-such-code") {
		t.Errorf("ログに Code が残るはず: %s", env.logs.String())
	}
}
