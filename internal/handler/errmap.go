package handler

import (
	"errors"
	"net/http"

	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// problemKindByCode は usecase の業務エラー(Code)を Problem Details の type・status に写す。
// 予約系の6種も入っている。殿が予約の usecase から usecase.Business(...) を返せば、そのまま正しい応答になる。
var problemKindByCode = map[usecase.Code]ProblemKind{
	usecase.CodeCapacityExceeded:           ProblemCapacityExceeded,
	usecase.CodeAlreadyReserved:            ProblemAlreadyReserved,
	usecase.CodeSlotHasActiveReservations:  ProblemSlotHasActiveReservations,
	usecase.CodeReservationDeadlinePassed:  ProblemReservationDeadlinePassed,
	usecase.CodeCancellationDeadlinePassed: ProblemCancellationDeadlinePassed,
	usecase.CodeRebookingNotAllowed:        ProblemRebookingNotAllowed,
	usecase.CodeSlotNotReservable:          ProblemSlotNotReservable,
	usecase.CodeSlotHasReservationHistory:  ProblemSlotHasReservationHistory,
	usecase.CodeInvalidStateTransition:     ProblemInvalidStateTransition,
	usecase.CodeUpdateNotAllowed:           ProblemUpdateNotAllowed,
}

// writeError は usecase が返した error を Problem Details にして書く。
// 想定外の error(DB の障害など)は 500。内部の事情は本文に出さず、request_id つきでログに残す。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *usecase.ValidationError
	var be *usecase.BusinessError
	switch {
	case errors.As(err, &ve):
		fields := make([]FieldError, len(ve.Fields))
		for i, f := range ve.Fields {
			fields[i] = FieldError{Field: f.Field, Message: f.Message}
		}
		WriteValidationProblem(w, r, fields)
	case errors.As(err, &be):
		kind, ok := problemKindByCode[be.Code]
		if !ok { // 写像表の漏れ(試験で防ぐ)。業務エラーとして返せぬので 500。
			middleware.LoggerFrom(r.Context()).Error("未対応の業務エラー", "code", string(be.Code))
			InternalServerError(w, r)
			return
		}
		WriteProblem(w, r, kind, be.Detail)
	case errors.Is(err, usecase.ErrForbidden):
		WriteStatusProblem(w, r, http.StatusForbidden, "")
	case errors.Is(err, usecase.ErrNotFound):
		NotFound(w, r)
	default:
		middleware.LoggerFrom(r.Context()).Error("想定外のエラー", "error", err)
		InternalServerError(w, r)
	}
}
