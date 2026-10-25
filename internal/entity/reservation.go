package entity

import "time"

// ReservationStatus は予約の状態。DB の reservations.status の値と同じ文字列。
type ReservationStatus string

const (
	ReservationActive           ReservationStatus = "active"             // 有効。定員の判定で数える
	ReservationCancelledByUser  ReservationStatus = "cancelled_by_user"  // 本人取消
	ReservationCancelledByAdmin ReservationStatus = "cancelled_by_admin" // 代理取消
)

// Valid は、s が定められた3つの値のどれかであること。
func (s ReservationStatus) Valid() bool {
	switch s {
	case ReservationActive, ReservationCancelledByUser, ReservationCancelledByAdmin:
		return true
	}
	return false
}

// Reservation は予約。殿が予約の usecase・repository を書くときに使う。
type Reservation struct {
	ID        int64
	PublicID  string
	SlotID    int64
	UserID    int64
	Status    ReservationStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}
