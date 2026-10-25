package usecase

import (
	"errors"
	"fmt"
)

// 型のない失敗。handler が 403・404 の Problem Details(about:blank)に写す。
var (
	// ErrForbidden は、権限が足りない(利用者が管理者専用の操作を呼んだ)。
	ErrForbidden = errors.New("権限がない")
	// ErrNotFound は、対象が無い、または見る権限がない(他人の予約・利用者から見た下書きの枠)。存在を漏らさぬため区別しない。
	ErrNotFound = errors.New("対象が見つからない")
)

// Code は業務エラーの種類。handler が /problems/<code> の type と status に写す。
// 予約系の6種は殿が使う。ここに定義済みなので、handler の写像を触らずに返せる。
type Code string

const (
	CodeCapacityExceeded           Code = "capacity-exceeded"            // 409 定員超過(予約系)
	CodeAlreadyReserved            Code = "already-reserved"             // 409 重複予約(予約系)
	CodeSlotHasActiveReservations  Code = "slot-has-active-reservations" // 409 有効な予約がある枠を閉じる
	CodeReservationDeadlinePassed  Code = "reservation-deadline-passed"  // 422 締切後の予約(予約系)
	CodeCancellationDeadlinePassed Code = "cancellation-deadline-passed" // 422 取消期限切れ(予約系)
	CodeRebookingNotAllowed        Code = "rebooking-not-allowed"        // 422 代理取消された枠の再予約(予約系)
	CodeSlotNotReservable          Code = "slot-not-reservable"          // 422 閉じた枠への予約(予約系)
	CodeSlotHasReservationHistory  Code = "slot-has-reservation-history" // 422 予約が入った枠の削除
	CodeInvalidStateTransition     Code = "invalid-state-transition"     // 422 許されない状態遷移
	CodeUpdateNotAllowed           Code = "update-not-allowed"           // 422 予約がある枠で変更できない項目
)

// AllBusinessCodes は定義済みの業務エラー10種(validation-error は ValidationError で別に扱う)。
// handler の写像表が漏れなく持つことを、試験で確かめるために使う。
func AllBusinessCodes() []Code {
	return []Code{
		CodeCapacityExceeded, CodeAlreadyReserved, CodeSlotHasActiveReservations,
		CodeReservationDeadlinePassed, CodeCancellationDeadlinePassed, CodeRebookingNotAllowed,
		CodeSlotNotReservable, CodeSlotHasReservationHistory, CodeInvalidStateTransition, CodeUpdateNotAllowed,
	}
}

// BusinessError は業務エラー(409・422)。errors.As で取り出す。
type BusinessError struct {
	Code   Code
	Detail string // 利用者に見せてよい説明(個人情報・内部事情を含めない)
}

func (e *BusinessError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Detail) }

// Business は業務エラーを作る。usecase はこれで返す: return usecase.Business(usecase.CodeCapacityExceeded, "定員に達している")
func Business(code Code, detail string) error { return &BusinessError{Code: code, Detail: detail} }

// IsBusiness は、err が業務エラー code であること(試験と usecase 内の判定用)。
func IsBusiness(err error, code Code) bool {
	var be *BusinessError
	return errors.As(err, &be) && be.Code == code
}

// FieldError は入力の誤りの1項目。Field は API の項目名(JSON の名前: title・startAt・limit など)。
type FieldError struct {
	Field   string
	Message string
}

// ValidationError は入力の誤り(400 validation-error)。errors.As で取り出す。
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "入力の誤り"
	}
	return fmt.Sprintf("入力の誤り: %s: %s", e.Fields[0].Field, e.Fields[0].Message)
}
