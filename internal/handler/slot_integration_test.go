package handler_test

// 通し試験: Router + 偽の認証 + SlotHandler + SlotService + SlotRepository + TxManager + 実 MySQL(testcontainers)。
//
// handler の httptest(slot_test.go)は usecase を偽物にし、usecase の単体試験は repository を偽物にしている。
// どちらも「本物の DB で本物の判定が出るか」は示さない。この試験は全層をつなぎ、
// 仕様書の業務エラー(枠系の4種)と出し分けを、本物の SELECT ... FOR UPDATE と本物の SQL で出す。
//
// 構成の要点:
//   - 認証は偽物。ヘッダ X-Test-Role(admin|user)と X-Test-Sub から Principal を context に入れる。
//     users 行は SQL で直接作り、Principal.UserID をその id に合わせる。
//   - 時刻は固定できる偽の Clock。R4 の境界(締切の1秒前・ちょうど・1秒後)を、時計を動かして試す。
//   - 予約の行は SQL で直接入れて前提を作る(予約の usecase・repository は殿が書く)。
//   - repotest.StartMySQL は1回に約11秒かかる。コンテナは1つだけ立て、repotest.Reset で場面を分ける。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/handler"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/repository"
	"github.com/shun2580/time-slot-reservation-api/internal/repository/repotest"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

const (
	integAdminSub = "it-admin"
	integUserSub  = "it-user"

	integProblemJSON = "application/problem+json"
	integAboutBlank  = "about:blank"

	// 場面の既定の時刻。時計は integBase、枠は 11/10 10:00〜11:00、予約締切はその前日 18:00。
	integBase     = "2026-11-01T09:00:00+09:00"
	integStart    = "2026-11-10T10:00:00+09:00"
	integEnd      = "2026-11-10T11:00:00+09:00"
	integDeadline = "2026-11-09T18:00:00+09:00"

	// 存在しない枠の ID(UUID の形だけ満たす)。
	integMissingID = "00000000-0000-4000-8000-000000000000"
)

// ---- 偽の Clock ----

type integClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *integClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *integClock) Set(t *testing.T, rfc3339 string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = integTime(t, rfc3339)
}

func integTime(t *testing.T, rfc3339 string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("時刻の指定が誤り: %q: %v", rfc3339, err)
	}
	return v.In(entity.JST)
}

// ---- 環境 ----

// integWho は誰として呼ぶか。空は認証ヘッダなし。
type integWho struct{ role, sub string }

var (
	integAsAdmin = integWho{"admin", integAdminSub}
	integAsUser  = integWho{"user", integUserSub}
)

type integEnv struct {
	db    *sql.DB
	clock *integClock
	h     http.Handler
	users map[string]int64 // sub → users.id(偽の認証が引く)
}

func newIntegEnv(t *testing.T) *integEnv {
	t.Helper()
	db := repotest.StartMySQL(t) // Docker が使えぬときは t.Fatal(Skip しない)
	e := &integEnv{db: db, clock: &integClock{}, users: map[string]int64{}}

	svc := usecase.NewSlotService(repository.NewSlotRepository(db), repository.NewTxManager(db), e.clock, entity.NewPublicID)
	r := handler.NewRouter()
	r.SetAuth(e.fakeAuth)
	handler.RegisterSlotRoutes(r, handler.NewSlotHandler(svc))
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	return e
}

// fakeAuth は本物の JWT 検証の代わり。ヘッダから Principal を作って context に入れる。
func (e *integEnv) fakeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, sub := r.Header.Get("X-Test-Role"), r.Header.Get("X-Test-Sub")
		id, known := e.users[sub]
		if (role != "admin" && role != "user") || !known {
			w.Header().Set("WWW-Authenticate", "Bearer")
			handler.WriteStatusProblem(w, r, http.StatusUnauthorized, "")
			return
		}
		p := middleware.Principal{Sub: sub, UserID: id, IsAdmin: role == "admin"}
		next.ServeHTTP(w, r.WithContext(middleware.ContextWithPrincipal(r.Context(), p)))
	})
}

// reset は表を空にし、時計を初期値に戻し、管理者と利用者の users 行を作り直す。場面の頭で呼ぶ。
func (e *integEnv) reset(t *testing.T) {
	t.Helper()
	repotest.Reset(t, e.db)
	e.users = map[string]int64{}
	e.clock.Set(t, integBase)
	e.user(t, integAdminSub)
	e.user(t, integUserSub)
}

// user は users に1行直接入れ(既にあれば再利用し)、その id を返す。
func (e *integEnv) user(t *testing.T, sub string) int64 {
	t.Helper()
	if id, ok := e.users[sub]; ok {
		return id
	}
	res, err := e.db.Exec(`INSERT INTO users (keycloak_sub, created_at, updated_at) VALUES (?, ?, ?)`, sub, e.clock.Now(), e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	e.users[sub] = id
	return id
}

// reserve は reservations に1行直接入れる。status は active・cancelled_by_user・cancelled_by_admin。
// 利用者ごとに1枠1予約(UNIQUE(slot_id, user_id))ゆえ、同じ枠に複数入れるときは sub を変える。
func (e *integEnv) reserve(t *testing.T, slotPublicID, sub string, status entity.ReservationStatus) {
	t.Helper()
	userID := e.user(t, sub)
	var slotID int64
	if err := e.db.QueryRow(`SELECT id FROM slots WHERE public_id = ?`, slotPublicID).Scan(&slotID); err != nil {
		t.Fatalf("予約を入れる枠が DB に無い: %s: %v", slotPublicID, err)
	}
	if _, err := e.db.Exec(`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		entity.NewPublicID(), slotID, userID, string(status), e.clock.Now(), e.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func (e *integEnv) do(who integWho, method, target, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if who.role != "" {
		req.Header.Set("X-Test-Role", who.role)
		req.Header.Set("X-Test-Sub", who.sub)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// ---- 応答の読み取りと断言 ----

type integSlot struct {
	ID                    string `json:"id"`
	Title                 string `json:"title"`
	Capacity              int    `json:"capacity"`
	Remaining             int    `json:"remaining"`
	StartAt               string `json:"startAt"` // 文字列のまま持つ(+09:00 で返ることを断言するため)
	EndAt                 string `json:"endAt"`
	ReservationDeadlineAt string `json:"reservationDeadlineAt"`
	Status                string `json:"status"`
}

type integList struct {
	Items  []integSlot `json:"items"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

func (l integList) titles() []string {
	out := []string{}
	for _, s := range l.Items {
		out = append(out, s.Title)
	}
	return out
}

type integProblem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Errors   []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (p integProblem) fields() []string {
	out := []string{}
	for _, e := range p.Errors {
		out = append(out, e.Field)
	}
	sort.Strings(out)
	return out
}

func integWantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, want, rec.Body.String())
	}
}

func integWantNoContent(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	integWantStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("204 の本文が空でない: %q", rec.Body.String())
	}
}

// integWantProblem は Problem Details の応答を確かめ、本文を返す。
// Content-Type が application/problem+json、本文の type・status が応答と一致することまで見る。
func integWantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) integProblem {
	t.Helper()
	integWantStatus(t, rec, status)
	if ct := rec.Header().Get("Content-Type"); ct != integProblemJSON {
		t.Errorf("Content-Type = %q, want %q", ct, integProblemJSON)
	}
	var p integProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("本文が Problem Details の JSON でない: %v\n%s", err, rec.Body.String())
	}
	if p.Type != typ {
		t.Errorf("type = %q, want %q", p.Type, typ)
	}
	if p.Status != status {
		t.Errorf("本文の status = %d, want %d", p.Status, status)
	}
	if p.Title == "" {
		t.Errorf("title が空")
	}
	return p
}

func integDecodeSlot(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) integSlot {
	t.Helper()
	integWantStatus(t, rec, wantStatus)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var s integSlot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("枠の JSON でない: %v\n%s", err, rec.Body.String())
	}
	return s
}

func integDecodeList(t *testing.T, rec *httptest.ResponseRecorder) integList {
	t.Helper()
	integWantStatus(t, rec, http.StatusOK)
	var l integList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("一覧の JSON でない: %v\n%s", err, rec.Body.String())
	}
	return l
}

func integEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- 本文の組み立て ----

// integOmit を値にすると、その項目を本文から外す。
type integOmitT struct{}

var integOmit = integOmitT{}

// integSlotJSON は既定の枠(定員 3、11/10 10:00〜11:00、締切は前日 18:00)に over を重ねた作成の本文を返す。
func integSlotJSON(over map[string]any) string {
	m := map[string]any{
		"title": "朝の枠", "capacity": 3,
		"startAt": integStart, "endAt": integEnd, "reservationDeadlineAt": integDeadline,
	}
	for k, v := range over {
		if v == integOmit {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return integJSON(m)
}

func integJSON(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// create は管理者として枠を作り(201)、その応答を返す。
func (e *integEnv) create(t *testing.T, over map[string]any) integSlot {
	t.Helper()
	return integDecodeSlot(t, e.do(integAsAdmin, http.MethodPost, "/slots", integSlotJSON(over)), http.StatusCreated)
}

// createPublished は枠を作って公開する。
func (e *integEnv) createPublished(t *testing.T, over map[string]any) integSlot {
	t.Helper()
	s := e.create(t, over)
	integWantNoContent(t, e.do(integAsAdmin, http.MethodPost, "/slots/"+s.ID+"/publish", ""))
	s.Status = "published"
	return s
}

func (e *integEnv) get(t *testing.T, who integWho, id string) integSlot {
	t.Helper()
	return integDecodeSlot(t, e.do(who, http.MethodGet, "/slots/"+id, ""), http.StatusOK)
}

func (e *integEnv) list(t *testing.T, who integWho, query string) integList {
	t.Helper()
	target := "/slots"
	if query != "" {
		target += "?" + query
	}
	return integDecodeList(t, e.do(who, http.MethodGet, target, ""))
}

// ---- DB の直接確認 ----

func (e *integEnv) dbStatus(t *testing.T, publicID string) string {
	t.Helper()
	var st string
	if err := e.db.QueryRow(`SELECT status FROM slots WHERE public_id = ?`, publicID).Scan(&st); err != nil {
		t.Fatalf("枠 %s を DB から読めない: %v", publicID, err)
	}
	return st
}

func (e *integEnv) countSlots(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM slots`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dbWork は MySQL が実行した文の累計(Com_*)を返す。「DB に触れぬ」ことを、前後の差で示すのに使う。
func (e *integEnv) dbWork(t *testing.T) map[string]int64 {
	t.Helper()
	rows, err := e.db.Query(`SHOW GLOBAL STATUS WHERE Variable_name IN
		('Com_select', 'Com_insert', 'Com_update', 'Com_delete', 'Com_begin', 'Com_commit', 'Com_rollback')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 7 {
		t.Fatalf("Com_* が7件取れない: %v", out)
	}
	return out
}

func integSameWork(a, b map[string]int64) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---- 試験本体 ----

// TestSlotIntegration は全層の通し試験。コンテナは1つで、場面ごとに Reset する。
func TestSlotIntegration(t *testing.T) {
	e := newIntegEnv(t)

	t.Run("flow", func(t *testing.T) { e.reset(t); integFlow(t, e) })
	t.Run("business_errors", func(t *testing.T) { e.reset(t); integBusinessErrors(t, e) })
	t.Run("patch_rules", func(t *testing.T) { e.reset(t); integPatchRules(t, e) })
	t.Run("visibility", func(t *testing.T) { e.reset(t); integVisibility(t, e) })
	t.Run("list_params", func(t *testing.T) { e.reset(t); integListParams(t, e) })
	t.Run("authz", func(t *testing.T) { e.reset(t); integAuthz(t, e) })
	t.Run("validation", func(t *testing.T) { e.reset(t); integValidation(t, e) })
}

// 場面1: 作成 → 利用者には見えない(下書き) → 公開 → 見える → 閉じる → 削除 → 消える。
func integFlow(t *testing.T, e *integEnv) {
	// 日時は Z や小数秒で渡す。JST・秒精度に直して保存され、+09:00 で返る(R12)。DB を通した値で確かめる。
	body := integSlotJSON(map[string]any{
		"startAt": "2026-11-10T01:00:00.900Z", "endAt": "2026-11-10T02:00:00Z", "reservationDeadlineAt": "2026-11-09T09:00:00Z",
	})
	rec := e.do(integAsAdmin, http.MethodPost, "/slots", body)
	created := integDecodeSlot(t, rec, http.StatusCreated)
	if _, ok := entity.ParsePublicID(created.ID); !ok {
		t.Fatalf("id が UUID の形でない: %q", created.ID)
	}
	if loc := rec.Header().Get("Location"); loc != "/slots/"+created.ID {
		t.Errorf("Location = %q, want %q", loc, "/slots/"+created.ID)
	}
	want := integSlot{
		ID: created.ID, Title: "朝の枠", Capacity: 3, Remaining: 3,
		StartAt: integStart, EndAt: integEnd, ReservationDeadlineAt: integDeadline, Status: "draft",
	}
	if created != want {
		t.Errorf("作成の応答\n got: %+v\nwant: %+v", created, want)
	}
	id := created.ID

	// 下書きは利用者に見えない: 一覧に出ず、詳細は 404(存在を漏らさない)。
	l := e.list(t, integAsUser, "")
	if len(l.Items) != 0 || l.Limit != 20 || l.Offset != 0 {
		t.Errorf("下書きが利用者の一覧に出た、または limit/offset が既定でない: %+v", l)
	}
	if rec := e.do(integAsUser, http.MethodGet, "/slots", ""); !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("0件の items が [] でない: %s", rec.Body.String())
	}
	integWantProblem(t, e.do(integAsUser, http.MethodGet, "/slots/"+id, ""), http.StatusNotFound, integAboutBlank)

	// 管理者には見える(DB を通した値が作成時の応答と一致する)。
	if got := e.get(t, integAsAdmin, id); got != want {
		t.Errorf("管理者の詳細\n got: %+v\nwant: %+v", got, want)
	}
	if l := e.list(t, integAsAdmin, ""); len(l.Items) != 1 || l.Items[0] != want {
		t.Errorf("管理者の一覧に下書きが出ない: %+v", l)
	}

	// 公開 → 利用者の一覧に出る(残席は定員と同じ)。
	integWantNoContent(t, e.do(integAsAdmin, http.MethodPost, "/slots/"+id+"/publish", ""))
	want.Status = "published"
	if l := e.list(t, integAsUser, ""); len(l.Items) != 1 || l.Items[0] != want {
		t.Errorf("公開した枠が利用者の一覧に出ない/値が違う: %+v", l)
	}
	if got := e.get(t, integAsUser, id); got != want || got.Remaining != got.Capacity {
		t.Errorf("利用者の詳細 = %+v, want %+v", got, want)
	}

	// 閉じる → 詳細は利用者にも見える(N7)が、一覧には出ない(published でない)。
	integWantNoContent(t, e.do(integAsAdmin, http.MethodPost, "/slots/"+id+"/close", ""))
	want.Status = "closed"
	if got := e.get(t, integAsUser, id); got != want {
		t.Errorf("閉じた枠の利用者の詳細 = %+v, want %+v", got, want)
	}
	if l := e.list(t, integAsUser, ""); len(l.Items) != 0 {
		t.Errorf("閉じた枠が利用者の一覧に出た: %+v", l)
	}
	if l := e.list(t, integAsAdmin, ""); len(l.Items) != 1 || l.Items[0].Status != "closed" {
		t.Errorf("閉じた枠が管理者の一覧に出ない: %+v", l)
	}

	// 削除(予約が一度も無い枠は、閉じた状態からでも消せる) → 管理者も利用者も 404。DB からも消える。
	integWantNoContent(t, e.do(integAsAdmin, http.MethodDelete, "/slots/"+id, ""))
	integWantProblem(t, e.do(integAsAdmin, http.MethodGet, "/slots/"+id, ""), http.StatusNotFound, integAboutBlank)
	integWantProblem(t, e.do(integAsUser, http.MethodGet, "/slots/"+id, ""), http.StatusNotFound, integAboutBlank)
	if n := e.countSlots(t); n != 0 {
		t.Errorf("削除後も slots に %d 行ある", n)
	}
}

// 場面2: 本物の DB・本物の FOR UPDATE で出る業務エラー(枠系の4種)。予約行は SQL で直接入れる。
func integBusinessErrors(t *testing.T, e *integEnv) {
	const (
		typActive   = "/problems/slot-has-active-reservations"
		typHistory  = "/problems/slot-has-reservation-history"
		typState    = "/problems/invalid-state-transition"
		typNotAllow = "/problems/update-not-allowed"
	)
	post := func(id, op string) *httptest.ResponseRecorder {
		return e.do(integAsAdmin, http.MethodPost, "/slots/"+id+"/"+op, "")
	}

	t.Run("409_close_with_active_reservation", func(t *testing.T) {
		s := e.createPublished(t, nil)
		e.reserve(t, s.ID, "res-a", entity.ReservationActive)
		p := integWantProblem(t, post(s.ID, "close"), http.StatusConflict, typActive)
		if p.Instance != "/slots/"+s.ID+"/close" {
			t.Errorf("instance = %q", p.Instance)
		}
		if st := e.dbStatus(t, s.ID); st != "published" {
			t.Errorf("409 の後に status が変わった: %s", st)
		}

		// 対比: 取消のみ(有効な予約が無い)の公開中の枠は閉じられる。閉じる判定は『有効な予約』だけを見る。
		c := e.createPublished(t, map[string]any{"title": "取消のみ"})
		e.reserve(t, c.ID, "res-b", entity.ReservationCancelledByUser)
		integWantNoContent(t, post(c.ID, "close"))
		if st := e.dbStatus(t, c.ID); st != "closed" {
			t.Errorf("取消のみの枠が閉じていない: %s", st)
		}
	})

	t.Run("422_delete_with_reservation_history", func(t *testing.T) {
		// R1: 取消済みも「予約が入った」に数える。状態は問わない(active・取消の2種・閉じた枠でも)。
		for _, st := range []entity.ReservationStatus{
			entity.ReservationCancelledByUser, entity.ReservationCancelledByAdmin, entity.ReservationActive,
		} {
			t.Run(string(st), func(t *testing.T) {
				s := e.createPublished(t, map[string]any{"title": "履歴あり-" + string(st)})
				e.reserve(t, s.ID, "res-"+string(st), st)
				p := integWantProblem(t, e.do(integAsAdmin, http.MethodDelete, "/slots/"+s.ID, ""), http.StatusUnprocessableEntity, typHistory)
				if p.Detail == "" {
					t.Errorf("detail が空")
				}
				if got := e.get(t, integAsAdmin, s.ID); got.ID != s.ID {
					t.Errorf("422 の後に枠が消えた/変わった: %+v", got)
				}
			})
		}
		// 取消のみの枠を閉じてから削除しても同じ(状態は問わない)。
		s := e.createPublished(t, map[string]any{"title": "閉じた履歴あり"})
		e.reserve(t, s.ID, "res-closed", entity.ReservationCancelledByUser)
		integWantNoContent(t, post(s.ID, "close"))
		integWantProblem(t, e.do(integAsAdmin, http.MethodDelete, "/slots/"+s.ID, ""), http.StatusUnprocessableEntity, typHistory)
	})

	t.Run("422_invalid_state_transition", func(t *testing.T) {
		draft := e.create(t, map[string]any{"title": "下書き"})
		pub := e.createPublished(t, map[string]any{"title": "公開"})
		closed := e.createPublished(t, map[string]any{"title": "閉じた"})
		integWantNoContent(t, post(closed.ID, "close"))

		rows := []struct {
			name   string
			rec    func() *httptest.ResponseRecorder
			wantDB string // 失敗の後の status(変わらぬこと)
			id     string
		}{
			{"close_draft", func() *httptest.ResponseRecorder { return post(draft.ID, "close") }, "draft", draft.ID},
			{"publish_published", func() *httptest.ResponseRecorder { return post(pub.ID, "publish") }, "published", pub.ID},
			{"publish_closed", func() *httptest.ResponseRecorder { return post(closed.ID, "publish") }, "closed", closed.ID},
			{"close_closed", func() *httptest.ResponseRecorder { return post(closed.ID, "close") }, "closed", closed.ID},
			{"patch_closed", func() *httptest.ResponseRecorder {
				return e.do(integAsAdmin, http.MethodPatch, "/slots/"+closed.ID, `{"title":"変えたい"}`)
			}, "closed", closed.ID},
		}
		for _, r := range rows {
			t.Run(r.name, func(t *testing.T) {
				integWantProblem(t, r.rec(), http.StatusUnprocessableEntity, typState)
				if st := e.dbStatus(t, r.id); st != r.wantDB {
					t.Errorf("422 の後に status が変わった: %s, want %s", st, r.wantDB)
				}
			})
		}
		if got := e.get(t, integAsAdmin, closed.ID); got.Title != "閉じた" {
			t.Errorf("閉じた枠の title が PATCH で変わった: %q", got.Title)
		}
	})

	t.Run("422_update_not_allowed", func(t *testing.T) {
		// 予約が1行でもある枠では title・startAt・endAt を変えられない。R1: 取消のみでも同じ。
		for _, st := range []entity.ReservationStatus{entity.ReservationActive, entity.ReservationCancelledByUser} {
			s := e.createPublished(t, map[string]any{"title": "変更不可-" + string(st)})
			e.reserve(t, s.ID, "res-"+string(st), st)
			for _, body := range []string{
				`{"title":"新しい題"}`,
				`{"startAt":"2026-11-10T10:30:00+09:00"}`,
				`{"endAt":"2026-11-10T12:00:00+09:00"}`,
				`{"title":"新しい題","capacity":10}`, // 許される項目が混じっていても、title があれば全体が拒まれる
			} {
				t.Run(string(st)+"_"+body, func(t *testing.T) {
					integWantProblem(t, e.do(integAsAdmin, http.MethodPatch, "/slots/"+s.ID, body), http.StatusUnprocessableEntity, typNotAllow)
				})
			}
			if got := e.get(t, integAsAdmin, s.ID); got.Title != "変更不可-"+string(st) || got.Capacity != 3 {
				t.Errorf("422 の後に枠が変わった: %+v", got)
			}
		}
	})
}

// 場面3: 予約のある枠で PATCH が許す変更・許さぬ変更(R1〜R3)。
func integPatchRules(t *testing.T, e *integEnv) {
	patch := func(id, body string) *httptest.ResponseRecorder {
		return e.do(integAsAdmin, http.MethodPatch, "/slots/"+id, body)
	}
	const typNotAllow = "/problems/update-not-allowed"

	t.Run("with_reservation", func(t *testing.T) {
		s := e.createPublished(t, map[string]any{"capacity": 5})
		e.reserve(t, s.ID, "res-1", entity.ReservationActive)
		e.reserve(t, s.ID, "res-2", entity.ReservationActive)

		// 定員を増やす → 204。残席は 新しい定員 − 有効な予約数。
		integWantNoContent(t, patch(s.ID, `{"capacity":6}`))
		if got := e.get(t, integAsAdmin, s.ID); got.Capacity != 6 || got.Remaining != 4 {
			t.Errorf("定員増の後 = capacity %d remaining %d, want 6/4", got.Capacity, got.Remaining)
		}
		// 締切を延ばす(開始以前まで)→ 204。
		integWantNoContent(t, patch(s.ID, `{"reservationDeadlineAt":"2026-11-10T09:00:00+09:00"}`))
		if got := e.get(t, integAsAdmin, s.ID); got.ReservationDeadlineAt != "2026-11-10T09:00:00+09:00" {
			t.Errorf("締切延長の後 = %s", got.ReservationDeadlineAt)
		}
		// 同値は無変更として通す(R2)。
		integWantNoContent(t, patch(s.ID, `{"capacity":6}`))
		integWantNoContent(t, patch(s.ID, `{"reservationDeadlineAt":"2026-11-10T09:00:00+09:00"}`))
		integWantNoContent(t, patch(s.ID, `{"capacity":6,"reservationDeadlineAt":"2026-11-10T00:00:00Z"}`)) // 同じ時刻を別の書き方で

		// 定員を減らす → 422(1つ減らすだけでも)。
		integWantProblem(t, patch(s.ID, `{"capacity":5}`), http.StatusUnprocessableEntity, typNotAllow)
		// 締切を縮める → 422(1秒でも)。
		integWantProblem(t, patch(s.ID, `{"reservationDeadlineAt":"2026-11-10T08:59:59+09:00"}`), http.StatusUnprocessableEntity, typNotAllow)
		// 同じ値でも startAt が本文にあれば 422(『のいずれかが本文にある』が基準。R2)。
		integWantProblem(t, patch(s.ID, `{"startAt":"`+integStart+`"}`), http.StatusUnprocessableEntity, typNotAllow)

		// 締切を開始より後へ延ばすのは、許される方向(422 でない)が項目間の整合に反する → 400(R3)。
		p := integWantProblem(t, patch(s.ID, `{"reservationDeadlineAt":"2026-11-10T10:00:01+09:00"}`), http.StatusBadRequest, "/problems/validation-error")
		if f := p.fields(); !integEqual(f, []string{"reservationDeadlineAt"}) {
			t.Errorf("errors[].field = %v", f)
		}

		// 失敗した変更は何も残さない: 最後の成功時の値のまま。
		got := e.get(t, integAsAdmin, s.ID)
		if got.Capacity != 6 || got.ReservationDeadlineAt != "2026-11-10T09:00:00+09:00" || got.Title != "朝の枠" || got.Status != "published" {
			t.Errorf("失敗した PATCH の後に値が変わった: %+v", got)
		}
	})

	t.Run("without_reservation", func(t *testing.T) {
		s := e.create(t, map[string]any{"capacity": 5})
		// 予約が無ければ全項目を変えられる(定員を減らす・開始を動かすも可)。updated_at は時計の値になる。
		e.clock.Set(t, "2026-11-02T12:34:56+09:00")
		integWantNoContent(t, patch(s.ID, `{"title":"新しい題","capacity":2,"startAt":"2026-11-10T13:00:00+09:00","endAt":"2026-11-10T14:00:00+09:00","reservationDeadlineAt":"2026-11-10T12:00:00+09:00"}`))
		want := integSlot{
			ID: s.ID, Title: "新しい題", Capacity: 2, Remaining: 2,
			StartAt: "2026-11-10T13:00:00+09:00", EndAt: "2026-11-10T14:00:00+09:00", ReservationDeadlineAt: "2026-11-10T12:00:00+09:00", Status: "draft",
		}
		if got := e.get(t, integAsAdmin, s.ID); got != want {
			t.Errorf("全項目変更の後\n got: %+v\nwant: %+v", got, want)
		}
		var updatedAt, createdAt time.Time
		if err := e.db.QueryRow(`SELECT updated_at, created_at FROM slots WHERE public_id = ?`, s.ID).Scan(&updatedAt, &createdAt); err != nil {
			t.Fatal(err)
		}
		if !updatedAt.Equal(integTime(t, "2026-11-02T12:34:56+09:00")) {
			t.Errorf("updated_at = %v, want 時計の値", updatedAt)
		}
		if !createdAt.Equal(integTime(t, integBase)) {
			t.Errorf("created_at = %v, want 作成時の時計の値", createdAt)
		}

		// 変更後の値で項目間を検証する(R3)。開始だけ終了より後へ動かすと endAt の誤り。400 で、何も書かれない。
		p := integWantProblem(t, patch(s.ID, `{"startAt":"2026-11-10T15:00:00+09:00"}`), http.StatusBadRequest, "/problems/validation-error")
		if f := p.fields(); !integEqual(f, []string{"endAt"}) {
			t.Errorf("errors[].field = %v", f)
		}
		if got := e.get(t, integAsAdmin, s.ID); got != want {
			t.Errorf("400 の後に値が変わった: %+v", got)
		}
		// 予約が無ければ定員を減らす PATCH も 204 のまま(上で確認済み)。1 まで下げても可。
		integWantNoContent(t, patch(s.ID, `{"capacity":1}`))
	})
}

// 場面4: 利用者に見える範囲(R4)と availableOnly。
func integVisibility(t *testing.T, e *integEnv) {
	t.Run("deadline_boundary_R4", func(t *testing.T) {
		// 利用者の一覧は『published かつ 現在時刻 < 締切』。締切ちょうどは締切後(出ない)。
		s := e.createPublished(t, map[string]any{"title": "締切の境界"})
		for _, c := range []struct {
			now  string
			want bool
		}{
			{"2026-11-09T17:59:59+09:00", true},
			{"2026-11-09T18:00:00+09:00", false},
			{"2026-11-09T18:00:01+09:00", false},
		} {
			t.Run(c.now, func(t *testing.T) {
				e.clock.Set(t, c.now)
				want := []string{}
				if c.want {
					want = []string{"締切の境界"}
				}
				if got := e.list(t, integAsUser, "").titles(); !integEqual(got, want) {
					t.Errorf("利用者の一覧 = %v, want %v", got, want)
				}
				// 管理者の一覧には時刻に関係なく出る。利用者の詳細も出る(N7: 締切後の published は 200)。
				if got := e.list(t, integAsAdmin, "").titles(); !integEqual(got, []string{"締切の境界"}) {
					t.Errorf("管理者の一覧 = %v", got)
				}
				if g := e.get(t, integAsUser, s.ID); g.Status != "published" {
					t.Errorf("利用者の詳細 = %+v", g)
				}
			})
		}
	})

	t.Run("available_only", func(t *testing.T) {
		// 上の小場面の枠を消し、時計も戻す。
		e.reset(t)
		// 開始時刻だけ変える(並びを固定するため。締切はそのまま)。
		at := func(h int) map[string]any {
			return map[string]any{
				"startAt": fmt.Sprintf("2026-11-10T%02d:00:00+09:00", h), "endAt": fmt.Sprintf("2026-11-10T%02d:30:00+09:00", h),
			}
		}
		with := func(over map[string]any, title string, capacity int) map[string]any {
			over["title"], over["capacity"] = title, capacity
			return over
		}
		full := e.createPublished(t, with(at(10), "満杯", 2))
		half := e.createPublished(t, with(at(11), "残り1", 2))
		cancelled := e.createPublished(t, with(at(12), "取消のみ", 1))
		e.create(t, with(at(13), "下書き空き", 5))
		e.reserve(t, full.ID, "av-1", entity.ReservationActive)
		e.reserve(t, full.ID, "av-2", entity.ReservationActive)
		e.reserve(t, half.ID, "av-3", entity.ReservationActive)
		e.reserve(t, cancelled.ID, "av-4", entity.ReservationCancelledByUser) // 取消は残席に数えない

		remaining := func(l integList) []int {
			out := []int{}
			for _, s := range l.Items {
				out = append(out, s.Remaining)
			}
			return out
		}
		check := func(name string, who integWho, query string, wantTitles []string, wantRemaining []int) {
			t.Run(name, func(t *testing.T) {
				l := e.list(t, who, query)
				if !integEqual(l.titles(), wantTitles) {
					t.Errorf("titles = %v, want %v", l.titles(), wantTitles)
				}
				if got := remaining(l); fmt.Sprint(got) != fmt.Sprint(wantRemaining) {
					t.Errorf("remaining = %v, want %v", got, wantRemaining)
				}
			})
		}
		// 利用者: 満杯の枠も(availableOnly でなければ)出る。残席 0。
		check("user_all", integAsUser, "", []string{"満杯", "残り1", "取消のみ"}, []int{0, 1, 1})
		check("user_available_only", integAsUser, "availableOnly=true", []string{"残り1", "取消のみ"}, []int{1, 1})
		check("user_available_only_false", integAsUser, "availableOnly=false", []string{"満杯", "残り1", "取消のみ"}, []int{0, 1, 1})
		// 管理者: availableOnly は『残席>0』だけを足す(状態は別軸。下書きの空きは残る)。
		check("admin_all", integAsAdmin, "", []string{"満杯", "残り1", "取消のみ", "下書き空き"}, []int{0, 1, 1, 5})
		check("admin_available_only", integAsAdmin, "availableOnly=true", []string{"残り1", "取消のみ", "下書き空き"}, []int{1, 1, 5})
		// 管理者の status 絞り込み。利用者の status は無視される。
		check("admin_status_draft", integAsAdmin, "status=draft", []string{"下書き空き"}, []int{5})
		check("admin_status_published_available", integAsAdmin, "status=published&availableOnly=true", []string{"残り1", "取消のみ"}, []int{1, 1})
		check("user_status_ignored", integAsUser, "status=draft", []string{"満杯", "残り1", "取消のみ"}, []int{0, 1, 1})
	})
}

// 場面5: limit・offset・from・to と並び(start_at, id の昇順。R16)。
func integListParams(t *testing.T, e *integEnv) {
	mk := func(title string, h int) {
		e.createPublished(t, map[string]any{
			"title":   title,
			"startAt": fmt.Sprintf("2026-11-10T%02d:00:00+09:00", h), "endAt": fmt.Sprintf("2026-11-10T%02d:45:00+09:00", h),
		})
	}
	// 作成順は並びと違えておく。s2a と s2b は同じ開始時刻で、id の昇順(作成順)に並ぶ。
	mk("s3", 12)
	mk("s1", 10)
	mk("s2a", 11)
	mk("s5", 14)
	mk("s2b", 11)
	mk("s4", 13)
	e.create(t, map[string]any{"title": "d6", "startAt": "2026-11-10T15:00:00+09:00", "endAt": "2026-11-10T15:45:00+09:00"})

	enc := url.QueryEscape
	rows := []struct {
		name       string
		query      string
		want       []string
		limit, off int
	}{
		{"default", "", []string{"s1", "s2a", "s2b", "s3", "s4", "s5"}, 20, 0},
		{"limit2", "limit=2", []string{"s1", "s2a"}, 2, 0},
		{"limit2_offset2", "limit=2&offset=2", []string{"s2b", "s3"}, 2, 2},
		{"offset5", "offset=5", []string{"s5"}, 20, 5},
		{"offset_past_end", "offset=6", []string{}, 20, 6},
		{"limit100", "limit=100", []string{"s1", "s2a", "s2b", "s3", "s4", "s5"}, 100, 0},
		{"limit1", "limit=1", []string{"s1"}, 1, 0},
		// from は start_at >= 、to は start_at < 。
		{"from_inclusive", "from=" + enc("2026-11-10T11:00:00+09:00"), []string{"s2a", "s2b", "s3", "s4", "s5"}, 20, 0},
		{"from_after_by_1s", "from=" + enc("2026-11-10T11:00:01+09:00"), []string{"s3", "s4", "s5"}, 20, 0},
		{"to_exclusive", "to=" + enc("2026-11-10T13:00:00+09:00"), []string{"s1", "s2a", "s2b", "s3"}, 20, 0},
		{"to_after_by_1s", "to=" + enc("2026-11-10T13:00:01+09:00"), []string{"s1", "s2a", "s2b", "s3", "s4"}, 20, 0},
		{"from_to", "from=" + enc("2026-11-10T11:00:00+09:00") + "&to=" + enc("2026-11-10T13:00:00+09:00"), []string{"s2a", "s2b", "s3"}, 20, 0},
		{"from_to_other_offset", "from=" + enc("2026-11-10T02:00:00Z") + "&to=" + enc("2026-11-10T04:00:00Z"), []string{"s2a", "s2b", "s3"}, 20, 0}, // Z で渡しても JST の 11:00〜13:00
		{"from_equals_to_is_empty", "from=" + enc("2026-11-10T11:00:00+09:00") + "&to=" + enc("2026-11-10T11:00:00+09:00"), []string{}, 20, 0},
		{"from_to_limit_offset", "from=" + enc("2026-11-10T11:00:00+09:00") + "&limit=2&offset=1", []string{"s2b", "s3"}, 2, 1},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			l := e.list(t, integAsUser, r.query)
			if !integEqual(l.titles(), r.want) {
				t.Errorf("titles = %v, want %v", l.titles(), r.want)
			}
			if l.Limit != r.limit || l.Offset != r.off {
				t.Errorf("limit/offset = %d/%d, want %d/%d", l.Limit, l.Offset, r.limit, r.off)
			}
		})
	}
	// 管理者の一覧は下書きを含む。
	if got := e.list(t, integAsAdmin, "").titles(); !integEqual(got, []string{"s1", "s2a", "s2b", "s3", "s4", "s5", "d6"}) {
		t.Errorf("管理者の一覧 = %v", got)
	}

	// 一覧の入力の誤り → 400 validation-error。errors[].field に誤った項目名。
	bad := []struct {
		name, query, field string
	}{
		{"limit0", "limit=0", "limit"},
		{"limit101", "limit=101", "limit"},
		{"limit_negative", "limit=-1", "limit"},
		{"limit_not_int", "limit=abc", "limit"},
		{"offset_negative", "offset=-1", "offset"},
		{"offset_not_int", "offset=1.5", "offset"},
		{"from_not_date", "from=yesterday", "from"},
		{"from_date_only", "from=2026-11-10", "from"},
		{"from_no_offset", "from=" + enc("2026-11-10T10:00:00"), "from"},
		{"to_not_date", "to=tomorrow", "to"},
		{"from_after_to", "from=" + enc("2026-11-10T12:00:00+09:00") + "&to=" + enc("2026-11-10T11:00:00+09:00"), "from"},
		{"available_only_bad", "availableOnly=yes", "availableOnly"},
	}
	for _, b := range bad {
		t.Run("400_"+b.name, func(t *testing.T) {
			p := integWantProblem(t, e.do(integAsUser, http.MethodGet, "/slots?"+b.query, ""), http.StatusBadRequest, "/problems/validation-error")
			if f := p.fields(); !integEqual(f, []string{b.field}) {
				t.Errorf("errors[].field = %v, want [%s]", f, b.field)
			}
		})
	}
	// 管理者の status が不正 → 400。利用者の status は無視されるので 200(上の user_status_ignored と同じ)。
	p := integWantProblem(t, e.do(integAsAdmin, http.MethodGet, "/slots?status=archived", ""), http.StatusBadRequest, "/problems/validation-error")
	if f := p.fields(); !integEqual(f, []string{"status"}) {
		t.Errorf("errors[].field = %v, want [status]", f)
	}
	integWantStatus(t, e.do(integAsUser, http.MethodGet, "/slots?status=archived", ""), http.StatusOK)
}

// 場面6: 利用者は管理者専用の操作を呼べない(403)。しかも DB に触れぬうちに断られる。
func integAuthz(t *testing.T, e *integEnv) {
	draft := e.create(t, map[string]any{"title": "下書き"})
	pub := e.createPublished(t, map[string]any{"title": "公開"})
	e.reserve(t, pub.ID, "az-1", entity.ReservationActive)

	rows := []struct {
		name, method, target, body string
	}{
		{"post_slots", http.MethodPost, "/slots", integSlotJSON(nil)},
		{"post_slots_broken_body", http.MethodPost, "/slots", "{"}, // 403 は 400 より先(R11)
		{"patch", http.MethodPatch, "/slots/" + draft.ID, `{"title":"乗っ取り"}`},
		{"patch_empty_body", http.MethodPatch, "/slots/" + draft.ID, `{}`},
		{"patch_missing_slot", http.MethodPatch, "/slots/" + integMissingID, `{"title":"x"}`}, // 403 は 404 より先
		{"delete", http.MethodDelete, "/slots/" + draft.ID, ""},
		{"delete_missing_slot", http.MethodDelete, "/slots/" + integMissingID, ""},
		{"delete_bad_id", http.MethodDelete, "/slots/not-a-uuid", ""}, // 403 は ID の形の検査(404)より先
		{"publish", http.MethodPost, "/slots/" + draft.ID + "/publish", ""},
		{"publish_missing_slot", http.MethodPost, "/slots/" + integMissingID + "/publish", ""},
		{"close", http.MethodPost, "/slots/" + pub.ID + "/close", ""},
		{"close_missing_slot", http.MethodPost, "/slots/" + integMissingID + "/close", ""},
	}

	// 計測の感度を確かめる(対照): 管理者の1回の変更は、MySQL の文の累計を確かに動かす。
	before := e.dbWork(t)
	integWantNoContent(t, e.do(integAsAdmin, http.MethodPatch, "/slots/"+draft.ID, `{"title":"下書き"}`))
	if integSameWork(before, e.dbWork(t)) {
		t.Fatalf("管理者の PATCH で Com_* が動かない: 『DB に触れぬ』の計測が空振りしている")
	}

	snapshot := e.snapshot(t)
	before = e.dbWork(t)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			integWantProblem(t, e.do(integAsUser, r.method, r.target, r.body), http.StatusForbidden, integAboutBlank)
		})
	}
	if after := e.dbWork(t); !integSameWork(before, after) {
		t.Errorf("403 の間に MySQL が文を実行した(DB に触れている)\nbefore: %v\n after: %v", before, after)
	}
	if got := e.snapshot(t); got != snapshot {
		t.Errorf("利用者の呼出しの後に DB が変わった\nbefore: %s\n after: %s", snapshot, got)
	}

	// 認証ヘッダが無い要求は、偽の認証が 401 で止める(handler まで届かぬ)。
	integWantProblem(t, e.do(integWho{}, http.MethodGet, "/slots", ""), http.StatusUnauthorized, integAboutBlank)
}

// snapshot は枠の全行を1本の文字列にする(変化の有無を比べる用)。
func (e *integEnv) snapshot(t *testing.T) string {
	t.Helper()
	rows, err := e.db.Query(`SELECT public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, updated_at FROM slots ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, title, status string
		var capacity int
		var a, b, c, d time.Time
		if err := rows.Scan(&id, &title, &capacity, &a, &b, &c, &status, &d); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "%s|%s|%d|%s|%s|%s|%s|%s\n", id, title, capacity, a, b, c, status, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

// 場面7: 入力の誤り(400)と、対象が無い(404)。いずれも DB を変えない。
func integValidation(t *testing.T, e *integEnv) {
	const typValidation = "/problems/validation-error"
	target := e.create(t, map[string]any{"title": "触られる枠"})
	before := e.snapshot(t)

	hundred := strings.Repeat("あ", 100) // 100 文字(バイト数は 300)。文字数で数える
	rows := []struct {
		name, method, path, body string
		want                     []string // errors[].field(昇順)。nil なら 400 と type だけ見る
	}{
		// 作成: 値の誤り
		{"create_title_empty", "POST", "/slots", integSlotJSON(map[string]any{"title": ""}), []string{"title"}},
		{"create_title_101", "POST", "/slots", integSlotJSON(map[string]any{"title": strings.Repeat("あ", 101)}), []string{"title"}},
		{"create_capacity_0", "POST", "/slots", integSlotJSON(map[string]any{"capacity": 0}), []string{"capacity"}},
		{"create_capacity_1001", "POST", "/slots", integSlotJSON(map[string]any{"capacity": 1001}), []string{"capacity"}},
		{"create_capacity_negative", "POST", "/slots", integSlotJSON(map[string]any{"capacity": -1}), []string{"capacity"}},
		{"create_start_equals_end", "POST", "/slots", integSlotJSON(map[string]any{"endAt": integStart}), []string{"endAt"}},
		{"create_start_after_end", "POST", "/slots", integSlotJSON(map[string]any{"startAt": integEnd, "endAt": integStart, "reservationDeadlineAt": integDeadline}), []string{"endAt"}},
		{"create_deadline_after_start", "POST", "/slots", integSlotJSON(map[string]any{"reservationDeadlineAt": "2026-11-10T10:00:01+09:00"}), []string{"reservationDeadlineAt"}},
		{"create_several_at_once", "POST", "/slots", integSlotJSON(map[string]any{"title": "", "capacity": 0}), []string{"capacity", "title"}},
		// 作成: 形の誤り
		{"create_bad_date", "POST", "/slots", integSlotJSON(map[string]any{"startAt": "2026-13-45T10:00:00+09:00"}), []string{"startAt"}},
		{"create_no_offset", "POST", "/slots", integSlotJSON(map[string]any{"startAt": "2026-11-10T10:00:00"}), []string{"startAt"}},
		{"create_date_only", "POST", "/slots", integSlotJSON(map[string]any{"endAt": "2026-11-10"}), []string{"endAt"}},
		{"create_missing_all", "POST", "/slots", `{}`, []string{"capacity", "endAt", "reservationDeadlineAt", "startAt", "title"}},
		{"create_missing_title", "POST", "/slots", integSlotJSON(map[string]any{"title": integOmit}), []string{"title"}},
		{"create_capacity_string", "POST", "/slots", integSlotJSON(map[string]any{"capacity": "3"}), nil},
		{"create_capacity_fraction", "POST", "/slots", integSlotJSON(map[string]any{"capacity": 2.5}), nil},
		{"create_unknown_field", "POST", "/slots", integSlotJSON(map[string]any{"status": "published"}), nil}, // 状態を作成時に指定させない
		{"create_broken_json", "POST", "/slots", "{", nil},
		{"create_trailing_garbage", "POST", "/slots", integSlotJSON(nil) + " {}", nil},
		// PATCH
		{"patch_empty_object", "PATCH", "/slots/" + target.ID, `{}`, nil},
		{"patch_array", "PATCH", "/slots/" + target.ID, `[]`, nil},
		{"patch_null_literal", "PATCH", "/slots/" + target.ID, `null`, nil},
		{"patch_null_field", "PATCH", "/slots/" + target.ID, `{"title":null}`, []string{"title"}},
		{"patch_status_not_allowed", "PATCH", "/slots/" + target.ID, `{"status":"closed"}`, []string{"status"}}, // 状態は専用の操作でしか変えられぬ
		{"patch_unknown_field", "PATCH", "/slots/" + target.ID, `{"title":"x","owner":"me"}`, []string{"owner"}},
		{"patch_title_empty", "PATCH", "/slots/" + target.ID, `{"title":""}`, []string{"title"}},
		{"patch_capacity_0", "PATCH", "/slots/" + target.ID, `{"capacity":0}`, []string{"capacity"}},
		{"patch_capacity_1001", "PATCH", "/slots/" + target.ID, `{"capacity":1001}`, []string{"capacity"}},
		{"patch_capacity_string", "PATCH", "/slots/" + target.ID, `{"capacity":"5"}`, []string{"capacity"}},
		{"patch_no_offset", "PATCH", "/slots/" + target.ID, `{"startAt":"2026-11-10T10:00:00"}`, []string{"startAt"}},
		{"patch_start_after_end", "PATCH", "/slots/" + target.ID, `{"startAt":"2026-11-10T12:00:00+09:00"}`, []string{"endAt"}},                                    // 現在値と重ねた後の整合(R3)
		{"patch_deadline_after_start", "PATCH", "/slots/" + target.ID, `{"reservationDeadlineAt":"2026-11-10T11:00:00+09:00"}`, []string{"reservationDeadlineAt"}}, // 同上
		{"patch_two_bad_fields", "PATCH", "/slots/" + target.ID, `{"title":"","capacity":0}`, []string{"capacity", "title"}},
		// 400 は 404 より先(R11): 存在しない枠でも本文の誤りは 400。
		{"patch_empty_object_missing_slot", "PATCH", "/slots/" + integMissingID, `{}`, nil},
		{"patch_capacity_0_missing_slot", "PATCH", "/slots/" + integMissingID, `{"capacity":0}`, []string{"capacity"}},
	}
	for _, r := range rows {
		t.Run("400_"+r.name, func(t *testing.T) {
			p := integWantProblem(t, e.do(integAsAdmin, r.method, r.path, r.body), http.StatusBadRequest, typValidation)
			if len(p.Errors) == 0 {
				t.Errorf("errors[] が空")
			}
			if r.want != nil {
				if f := p.fields(); !integEqual(f, r.want) {
					t.Errorf("errors[].field = %v, want %v", f, r.want)
				}
			}
			for _, fe := range p.Errors {
				if fe.Message == "" {
					t.Errorf("errors[] の message が空: %+v", fe)
				}
			}
		})
	}

	// 100 文字ちょうど(バイト数は 300)は通る。境界の内側が弾かれていないことの対照。
	ok := e.create(t, map[string]any{"title": hundred, "capacity": 1000})
	if ok.Title != hundred || ok.Capacity != 1000 {
		t.Errorf("境界値の枠が保存されていない: %+v", ok)
	}
	before = e.snapshot(t) // 境界値の枠を入れた後を基準にする

	// 対象が無い(404): 存在しない ID、UUID の形でない ID(R11)。
	notFound := []struct{ name, method, path, body string }{
		{"get_missing", "GET", "/slots/" + integMissingID, ""},
		{"get_bad_id", "GET", "/slots/not-a-uuid", ""},
		{"patch_missing", "PATCH", "/slots/" + integMissingID, `{"title":"x"}`},
		{"patch_bad_id", "PATCH", "/slots/not-a-uuid", `{"title":"x"}`},
		{"delete_missing", "DELETE", "/slots/" + integMissingID, ""},
		{"delete_bad_id", "DELETE", "/slots/not-a-uuid", ""},
		{"publish_missing", "POST", "/slots/" + integMissingID + "/publish", ""},
		{"publish_bad_id", "POST", "/slots/not-a-uuid/publish", ""},
		{"close_missing", "POST", "/slots/" + integMissingID + "/close", ""},
		{"close_bad_id", "POST", "/slots/not-a-uuid/close", ""},
	}
	for _, r := range notFound {
		t.Run("404_"+r.name, func(t *testing.T) {
			integWantProblem(t, e.do(integAsAdmin, r.method, r.path, r.body), http.StatusNotFound, integAboutBlank)
		})
	}

	// どの失敗も DB を変えていない。
	if after := e.snapshot(t); after != before {
		t.Errorf("400/404 の後に DB が変わった\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := e.countSlots(t); n != 2 {
		t.Errorf("slots の行数 = %d, want 2(触られる枠 + 境界値の枠)", n)
	}
}
