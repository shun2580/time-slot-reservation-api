package usecase

import (
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
)

// CreateSlotInput は枠の作成の入力。handler が必須項目の有無と日時の形を確かめてから渡す。
type CreateSlotInput struct {
	Title                 string
	Capacity              int
	StartAt               time.Time
	EndAt                 time.Time
	ReservationDeadlineAt time.Time
}

// UpdateSlotInput は枠の変更(PATCH)の入力。nil は「本文に無かった」項目。
type UpdateSlotInput struct {
	Title                 *string
	Capacity              *int
	StartAt               *time.Time
	EndAt                 *time.Time
	ReservationDeadlineAt *time.Time
}

// ListSlotsInput は枠の一覧の入力。Limit・Offset の nil は「指定なし」(既定 20・0)。
type ListSlotsInput struct {
	From          *time.Time
	To            *time.Time
	AvailableOnly bool
	Status        *entity.SlotStatus // 管理者のときだけ使う。利用者が指定しても無視する
	Limit         *int
	Offset        *int
}

// SlotPage は一覧の結果。Limit・Offset は実効値(既定を適用した後)。
type SlotPage struct {
	Items         []entity.SlotWithRemaining
	Limit, Offset int
}
