// Package handler は、HTTP の入口(ルーティング・入出力の変換)を置く。
package handler

import (
	"encoding/json"
	"net/http"
)

// ContentTypeProblem は、RFC 9457 の Problem Details の Content-Type。
const ContentTypeProblem = "application/problem+json"

// aboutBlank は、業務エラーではない応答(401・403・404・500・501)の type。
// api/openapi.yaml に定めがないための仮の扱い(RFC 9457 の既定値)。
const aboutBlank = "about:blank"

// Problem は、RFC 9457 の Problem Details の本文。
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// ProblemKind は、業務エラーの種類(type と status の組)。
type ProblemKind struct {
	Type   string
	Status int
	Title  string
}

// 業務エラーの type と status。api/openapi.yaml 冒頭の表の11件と一致させる(試験で突き合わせる)。
// type の文字列はここ以外に書かない。
var (
	ProblemCapacityExceeded           = ProblemKind{"/problems/capacity-exceeded", http.StatusConflict, "定員超過"}
	ProblemAlreadyReserved            = ProblemKind{"/problems/already-reserved", http.StatusConflict, "同じ利用者の重複予約"}
	ProblemSlotHasActiveReservations  = ProblemKind{"/problems/slot-has-active-reservations", http.StatusConflict, "有効な予約がある枠は閉じられない"}
	ProblemReservationDeadlinePassed  = ProblemKind{"/problems/reservation-deadline-passed", http.StatusUnprocessableEntity, "予約締切を過ぎている"}
	ProblemCancellationDeadlinePassed = ProblemKind{"/problems/cancellation-deadline-passed", http.StatusUnprocessableEntity, "取消期限を過ぎている"}
	ProblemRebookingNotAllowed        = ProblemKind{"/problems/rebooking-not-allowed", http.StatusUnprocessableEntity, "代理取消された枠は再予約できない"}
	ProblemSlotNotReservable          = ProblemKind{"/problems/slot-not-reservable", http.StatusUnprocessableEntity, "閉じた枠は予約できない"}
	ProblemSlotHasReservationHistory  = ProblemKind{"/problems/slot-has-reservation-history", http.StatusUnprocessableEntity, "予約が入った枠は削除できない"}
	ProblemInvalidStateTransition     = ProblemKind{"/problems/invalid-state-transition", http.StatusUnprocessableEntity, "許されない状態遷移"}
	ProblemUpdateNotAllowed           = ProblemKind{"/problems/update-not-allowed", http.StatusUnprocessableEntity, "予約がある枠では変更できない項目"}
	ProblemValidationError            = ProblemKind{"/problems/validation-error", http.StatusBadRequest, "入力の誤り"}
)

// BusinessProblemKinds は、業務エラー11件の一覧を返す。
func BusinessProblemKinds() []ProblemKind {
	return []ProblemKind{
		ProblemCapacityExceeded,
		ProblemAlreadyReserved,
		ProblemSlotHasActiveReservations,
		ProblemReservationDeadlinePassed,
		ProblemCancellationDeadlinePassed,
		ProblemRebookingNotAllowed,
		ProblemSlotNotReservable,
		ProblemSlotHasReservationHistory,
		ProblemInvalidStateTransition,
		ProblemUpdateNotAllowed,
		ProblemValidationError,
	}
}

// WriteProblem は、業務エラーを Problem Details で返す。detail は空でもよい。
func WriteProblem(w http.ResponseWriter, r *http.Request, kind ProblemKind, detail string) {
	writeProblem(w, r, Problem{Type: kind.Type, Title: kind.Title, Status: kind.Status, Detail: detail})
}

// WriteStatusProblem は、業務エラー以外(401・403・404・500・501 など)を Problem Details で返す。
// type は about:blank、title は HTTP のステータス文言(RFC 9457 の about:blank の定め)。
func WriteStatusProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	writeProblem(w, r, Problem{Type: aboutBlank, Title: http.StatusText(status), Status: status, Detail: detail})
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.Instance = r.URL.Path // クエリ文字列は含めない
	body, err := json.Marshal(p)
	if err != nil {
		// Problem は文字列と整数だけなので到達しない。
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// NotFound は、404 の Problem Details を返す。
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteStatusProblem(w, r, http.StatusNotFound, "")
}

// InternalServerError は、500 の Problem Details を返す。内部の事情は本文に出さない。
func InternalServerError(w http.ResponseWriter, r *http.Request) {
	WriteStatusProblem(w, r, http.StatusInternalServerError, "")
}

// NotImplemented は、501 の Problem Details を返す。予約系ルートの仮の受け皿(工程Dで置き換える)。
func NotImplemented(w http.ResponseWriter, r *http.Request) {
	WriteStatusProblem(w, r, http.StatusNotImplemented, "")
}
