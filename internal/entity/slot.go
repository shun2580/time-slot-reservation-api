package entity

import "time"

// SlotStatus は枠の状態。DB の slots.status の値と同じ文字列。
type SlotStatus string

const (
	SlotDraft     SlotStatus = "draft"     // 下書き。利用者には見えず、予約できない
	SlotPublished SlotStatus = "published" // 公開。締切前・定員未満なら予約できる
	SlotClosed    SlotStatus = "closed"    // 閉じた。再び開けられない
)

// Valid は、s が定められた3つの値のどれかであること。
func (s SlotStatus) Valid() bool {
	switch s {
	case SlotDraft, SlotPublished, SlotClosed:
		return true
	}
	return false
}

// Slot は時間枠。時刻はすべて JST の time.Time。
// 「締切後」「終了」は時刻から判定し、状態には持たない(仕様)。
type Slot struct {
	ID                    int64 // 内部の連番。API には出さない
	PublicID              string
	Title                 string
	Capacity              int
	StartAt               time.Time
	EndAt                 time.Time
	ReservationDeadlineAt time.Time // reservation_deadline_at
	Status                SlotStatus
	CreatedAt, UpdatedAt  time.Time
}

// SlotWithRemaining は、残席数つきの枠。一覧・詳細・作成の応答に使う。
// Remaining = Capacity − 有効な予約の数(R5)。draft・closed・締切後の枠でも同じ式で、予約できるかは Status と締切の別軸。
type SlotWithRemaining struct {
	Slot
	Remaining int
}
