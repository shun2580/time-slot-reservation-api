package usecase

import (
	"context"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
)

// LockedSlot は、SELECT ... FOR UPDATE で行ロックを取った枠。
//
// 「ロックは枠の行を先、予約の行は後」(仕様「同時実行」)を型で表すための値で、
// 予約の行を読み書きするメソッドは必ずこれを引数に取る(殿が書く SlotTx と同じ流儀の ReservationTx も)。
// LockByPublicID の戻り値としてだけ作ること。repository 以外・試験以外で組み立ててはならない。
type LockedSlot struct {
	Slot entity.Slot
}

// SlotListQuery は repository の一覧検索の条件。誰に何を見せるかは usecase が決めてここに落とす
// (repository は利用者・管理者を知らない)。
type SlotListQuery struct {
	ReservableAt  *time.Time         // 非 nil: status = published かつ reservation_deadline_at > この時刻(利用者の見える範囲)
	Status        *entity.SlotStatus // 非 nil: status = この値(管理者の絞り込み)
	From          *time.Time         // 非 nil: start_at >= From
	To            *time.Time         // 非 nil: start_at <  To
	AvailableOnly bool               // true: 有効な予約の数 < capacity
	Limit, Offset int                // 検証済みの値(Limit は 1〜100)
}

// SlotStore は、トランザクション外で使う枠の保存先。repository.SlotRepository が満たす。
type SlotStore interface {
	// Insert は枠を1行入れ、ID を埋めた枠を返す。
	Insert(ctx context.Context, s entity.Slot) (entity.Slot, error)
	// GetWithRemaining は public_id の枠を残席数つきで返す。無ければ ErrNotFound。
	GetWithRemaining(ctx context.Context, publicID string) (entity.SlotWithRemaining, error)
	// List は条件に合う枠を start_at, id の昇順で返す。
	List(ctx context.Context, q SlotListQuery) ([]entity.SlotWithRemaining, error)
}

// SlotTx は、トランザクションの中で使う枠の操作。必ず LockByPublicID を最初に呼ぶ。
type SlotTx interface {
	// LockByPublicID は枠の行を SELECT ... FOR UPDATE で取る。無ければ ErrNotFound。
	LockByPublicID(ctx context.Context, publicID string) (LockedSlot, error)
	// HasAnyReservation は、取消済みを含む全状態の予約が1つでもあるか(R1: 変更・削除の判定)。reservations は読むだけ。
	HasAnyReservation(ctx context.Context, s LockedSlot) (bool, error)
	// HasActiveReservation は、有効(active)な予約があるか(閉じる判定)。reservations は読むだけ。
	HasActiveReservation(ctx context.Context, s LockedSlot) (bool, error)
	// UpdateFields は title・capacity・start_at・end_at・reservation_deadline_at・updated_at を s.ID の行へ書く。
	UpdateFields(ctx context.Context, s LockedSlot, to entity.Slot) error
	// UpdateStatus は status と updated_at を書く。
	UpdateStatus(ctx context.Context, s LockedSlot, to entity.SlotStatus, at time.Time) error
	// Delete は枠を物理削除する。予約が1行でもあれば FK(RESTRICT)で失敗する(最後の防壁)。
	Delete(ctx context.Context, s LockedSlot) error
}

// Tx は、1つのトランザクションの中で使う repository の束。
// 殿が予約を書くときは、ここへ Reservations() ReservationTx を足す。
type Tx interface {
	Slots() SlotTx
}

// TxRunner は、トランザクションを張って fn を実行する。fn が error を返せば rollback し、その error をそのまま返す
// (errors.As で業務エラーを取り出せるように、包み直さない)。fn が panic しても rollback してから panic を続ける。
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}
