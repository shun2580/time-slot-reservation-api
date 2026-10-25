package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// SlotUsecase は SlotHandler が使う業務処理。*usecase.SlotService が満たす(試験では偽物を差す)。
type SlotUsecase interface {
	Create(ctx context.Context, actor usecase.Actor, in usecase.CreateSlotInput) (entity.SlotWithRemaining, error)
	Get(ctx context.Context, actor usecase.Actor, publicID string) (entity.SlotWithRemaining, error)
	List(ctx context.Context, actor usecase.Actor, in usecase.ListSlotsInput) (usecase.SlotPage, error)
	Update(ctx context.Context, actor usecase.Actor, publicID string, in usecase.UpdateSlotInput) error
	Delete(ctx context.Context, actor usecase.Actor, publicID string) error
	Publish(ctx context.Context, actor usecase.Actor, publicID string) error
	Close(ctx context.Context, actor usecase.Actor, publicID string) error
}

// SlotHandler は枠の7エンドポイント。入出力の変換だけを行い、業務規則は持たない。
type SlotHandler struct {
	uc SlotUsecase
}

func NewSlotHandler(uc SlotUsecase) *SlotHandler { return &SlotHandler{uc: uc} }

// RegisterSlotRoutes は枠の7本を Protected で登録する。
func RegisterSlotRoutes(r *Router, h *SlotHandler) {
	r.Protected("GET /slots", http.HandlerFunc(h.List))
	r.Protected("POST /slots", http.HandlerFunc(h.Create))
	r.Protected("GET /slots/{id}", http.HandlerFunc(h.Get))
	r.Protected("PATCH /slots/{id}", http.HandlerFunc(h.Update))
	r.Protected("DELETE /slots/{id}", http.HandlerFunc(h.Delete))
	r.Protected("POST /slots/{id}/publish", http.HandlerFunc(h.Publish))
	r.Protected("POST /slots/{id}/close", http.HandlerFunc(h.Close))
}

// actorFrom は認証ミドルウェアが入れた Principal から usecase.Actor を作る。
// Protected で登録されていれば必ずあるが、無ければ 401(配線の誤り)。
func actorFrom(w http.ResponseWriter, r *http.Request) (usecase.Actor, bool) {
	p, ok := middleware.PrincipalFrom(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		WriteStatusProblem(w, r, http.StatusUnauthorized, "")
		return usecase.Actor{}, false
	}
	return usecase.Actor{UserID: p.UserID, Sub: p.Sub, IsAdmin: p.IsAdmin}, true
}

// slotID は path の {id} を UUID の形として確かめる。形が違えば「存在しない」として 404(R11)。
func slotID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := entity.ParsePublicID(r.PathValue("id"))
	if !ok {
		NotFound(w, r)
		return "", false
	}
	return id, true
}

// ---- DTO(api/openapi.yaml の Slot・SlotCreate・SlotList に一致) ----

type slotResponse struct {
	ID                    string    `json:"id"`
	Title                 string    `json:"title"`
	Capacity              int       `json:"capacity"`
	Remaining             int       `json:"remaining"`
	StartAt               time.Time `json:"startAt"`
	EndAt                 time.Time `json:"endAt"`
	ReservationDeadlineAt time.Time `json:"reservationDeadlineAt"`
	Status                string    `json:"status"`
}

func toSlotResponse(s entity.SlotWithRemaining) slotResponse {
	return slotResponse{
		ID: s.PublicID, Title: s.Title, Capacity: s.Capacity, Remaining: s.Remaining,
		StartAt: s.StartAt.In(entity.JST), EndAt: s.EndAt.In(entity.JST), ReservationDeadlineAt: s.ReservationDeadlineAt.In(entity.JST),
		Status: string(s.Status),
	}
}

type slotListResponse struct {
	Items  []slotResponse `json:"items"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil { // DTO は文字列・整数・時刻だけなので到達しない
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---- 本文の読み取り(形の検証だけ。値の規則は usecase) ----

const maxBodyBytes = 1 << 20

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, []FieldError) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, []FieldError{{Field: "", Message: "本文を読めない(大きすぎる可能性)"}}
	}
	return b, nil
}

type slotCreateRequest struct {
	Title                 *string `json:"title"`
	Capacity              *int    `json:"capacity"`
	StartAt               *string `json:"startAt"` // 日時は文字列で受け、項目名つきで形の誤りを返す
	EndAt                 *string `json:"endAt"`
	ReservationDeadlineAt *string `json:"reservationDeadlineAt"`
}

// parseRFC3339 は RFC 3339(オフセット付き。Z も可)の日時を読む。オフセットの無い値は拒む(R12)。
func parseRFC3339(field, v string, errs *[]FieldError) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		*errs = append(*errs, FieldError{Field: field, Message: "RFC 3339 の日時(オフセット付き)で指定してください"})
	}
	return t
}

// decodeStrict は未知の項目を拒み、本文の末尾に余計な値が無いことを確かめて dst に読む。
func decodeStrict(b []byte, dst any) []FieldError {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return jsonErrorToFields(err)
	}
	// 末尾の確認は dec.More() ではなく「もう1回読んで EOF か」で行う。More() は後ろが } や ] だと false を返し、`{...} }` を通してしまう。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return []FieldError{{Field: "", Message: "JSON の後ろに余計な値がある"}}
	}
	return nil
}

func jsonErrorToFields(err error) []FieldError {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return []FieldError{{Field: te.Field, Message: "型が正しくない"}}
	}
	// 日時の形(RFC 3339・オフセット付き)の誤りは time.Time の UnmarshalJSON が返す。項目名は取れないので全体の誤りとして返す。
	return []FieldError{{Field: "", Message: "JSON として読めない、または未知の項目がある"}}
}

// ---- ハンドラ ----

func (h *SlotHandler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := usecase.RequireAdmin(actor); err != nil { // 403 は 400 より先(R11)
		writeError(w, r, err)
		return
	}
	body, errs := readBody(w, r)
	var req slotCreateRequest
	if errs == nil {
		errs = decodeStrict(body, &req)
	}
	if errs == nil {
		errs = requireCreateFields(req)
	}
	var in usecase.CreateSlotInput
	if errs == nil {
		in.Title, in.Capacity = *req.Title, *req.Capacity
		in.StartAt = parseRFC3339("startAt", *req.StartAt, &errs)
		in.EndAt = parseRFC3339("endAt", *req.EndAt, &errs)
		in.ReservationDeadlineAt = parseRFC3339("reservationDeadlineAt", *req.ReservationDeadlineAt, &errs)
	}
	if errs != nil {
		WriteValidationProblem(w, r, errs)
		return
	}
	out, err := h.uc.Create(r.Context(), actor, in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/slots/"+out.PublicID)
	writeJSON(w, http.StatusCreated, toSlotResponse(out))
}

func requireCreateFields(req slotCreateRequest) []FieldError {
	var errs []FieldError
	need := func(missing bool, field string) {
		if missing {
			errs = append(errs, FieldError{Field: field, Message: "必須です"})
		}
	}
	need(req.Title == nil, "title")
	need(req.Capacity == nil, "capacity")
	need(req.StartAt == nil, "startAt")
	need(req.EndAt == nil, "endAt")
	need(req.ReservationDeadlineAt == nil, "reservationDeadlineAt")
	return errs
}

func (h *SlotHandler) Get(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	id, ok := slotID(w, r)
	if !ok {
		return
	}
	out, err := h.uc.Get(r.Context(), actor, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSlotResponse(out))
}

func (h *SlotHandler) List(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	in, errs := parseListQuery(r)
	if errs != nil {
		WriteValidationProblem(w, r, errs)
		return
	}
	page, err := h.uc.List(r.Context(), actor, in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items := make([]slotResponse, len(page.Items)) // 0件でも [] を返す(null にしない)
	for i, s := range page.Items {
		items[i] = toSlotResponse(s)
	}
	writeJSON(w, http.StatusOK, slotListResponse{Items: items, Limit: page.Limit, Offset: page.Offset})
}

// parseListQuery はクエリの「形」(整数・真偽・RFC 3339・文字列)だけを確かめる。範囲は usecase。
func parseListQuery(r *http.Request) (usecase.ListSlotsInput, []FieldError) {
	q := r.URL.Query()
	var in usecase.ListSlotsInput
	var errs []FieldError

	parseTime := func(name string) *time.Time {
		v := q.Get(name)
		if v == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			errs = append(errs, FieldError{Field: name, Message: "RFC 3339 の日時(オフセット付き)で指定してください"})
			return nil
		}
		return &t
	}
	parseInt := func(name string) *int {
		v := q.Get(name)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, FieldError{Field: name, Message: "整数で指定してください"})
			return nil
		}
		return &n
	}
	in.From, in.To = parseTime("from"), parseTime("to")
	in.Limit, in.Offset = parseInt("limit"), parseInt("offset")
	if v := q.Get("availableOnly"); v != "" {
		switch v {
		case "true":
			in.AvailableOnly = true
		case "false":
		default:
			errs = append(errs, FieldError{Field: "availableOnly", Message: "true か false で指定してください"})
		}
	}
	if v := q.Get("status"); v != "" {
		st := entity.SlotStatus(v) // 値の妥当性(管理者のときだけ検査)は usecase
		in.Status = &st
	}
	return in, errs
}

// patchFields は PATCH で受ける項目。ここに無い項目・null は 400(R2)。
var patchFields = map[string]struct{}{"title": {}, "capacity": {}, "startAt": {}, "endAt": {}, "reservationDeadlineAt": {}}

// decodePatch は本文を「送られた項目だけ」の UpdateSlotInput にする。
// map[string]json.RawMessage で一度受けるので、「項目が無い」と「null」を区別できる。
func decodePatch(b []byte) (usecase.UpdateSlotInput, []FieldError) {
	var in usecase.UpdateSlotInput
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return in, []FieldError{{Field: "", Message: "JSON のオブジェクトで指定してください"}}
	}
	var errs []FieldError
	for name, v := range raw {
		if _, ok := patchFields[name]; !ok {
			errs = append(errs, FieldError{Field: name, Message: "変更できない項目です"})
			continue
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			errs = append(errs, FieldError{Field: name, Message: "null は指定できません"})
			continue
		}
		var err error
		switch name {
		case "title":
			in.Title, err = decodeAs[string](v)
		case "capacity":
			in.Capacity, err = decodeAs[int](v)
		case "startAt":
			in.StartAt, err = decodeAs[time.Time](v)
		case "endAt":
			in.EndAt, err = decodeAs[time.Time](v)
		case "reservationDeadlineAt":
			in.ReservationDeadlineAt, err = decodeAs[time.Time](v)
		}
		if err != nil {
			errs = append(errs, FieldError{Field: name, Message: "型または形が正しくない(日時は RFC 3339 でオフセット付き)"})
		}
	}
	sortFieldErrors(errs) // map の走査順は不定。応答を安定させる
	return in, errs
}

func decodeAs[T any](v json.RawMessage) (*T, error) {
	var out T
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (h *SlotHandler) Update(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	if err := usecase.RequireAdmin(actor); err != nil {
		writeError(w, r, err)
		return
	}
	body, errs := readBody(w, r)
	var in usecase.UpdateSlotInput
	if errs == nil {
		in, errs = decodePatch(body)
	}
	if errs != nil {
		WriteValidationProblem(w, r, errs)
		return
	}
	id, ok := slotID(w, r)
	if !ok {
		return
	}
	if err := h.uc.Update(r.Context(), actor, id, in); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// 本文の無い操作(削除・公開・閉じる)は共通の形: 認証 → 管理者 → ID の形 → usecase → 204。
func (h *SlotHandler) command(op func(ctx context.Context, actor usecase.Actor, id string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := actorFrom(w, r)
		if !ok {
			return
		}
		if err := usecase.RequireAdmin(actor); err != nil {
			writeError(w, r, err)
			return
		}
		id, ok := slotID(w, r)
		if !ok {
			return
		}
		if err := op(r.Context(), actor, id); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *SlotHandler) Delete(w http.ResponseWriter, r *http.Request)  { h.command(h.uc.Delete)(w, r) }
func (h *SlotHandler) Publish(w http.ResponseWriter, r *http.Request) { h.command(h.uc.Publish)(w, r) }
func (h *SlotHandler) Close(w http.ResponseWriter, r *http.Request)   { h.command(h.uc.Close)(w, r) }
