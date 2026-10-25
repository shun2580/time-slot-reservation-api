package usecase

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
)

const (
	titleMaxRunes = 100
	capacityMin   = 1
	capacityMax   = 1000
	defaultLimit  = 20
	maxLimit      = 100
)

// SlotService は枠の管理(作成・一覧・詳細・変更・公開・閉じる・削除)の業務規則を持つ。
// 認可・入力検証・状態遷移の条件はここに置く(handler には置かない)。
type SlotService struct {
	slots SlotStore
	tx    TxRunner
	clock Clock
	newID func() string
}

// NewSlotService は SlotService を作る。newID は API で見せる ID の採番(通常は entity.NewPublicID)。
func NewSlotService(slots SlotStore, tx TxRunner, clock Clock, newID func() string) *SlotService {
	return &SlotService{slots: slots, tx: tx, clock: clock, newID: newID}
}

// normalize は時刻を JST・秒精度にそろえる(R12: DATETIME は小数秒を持たず丸めるため、先に切り捨てて検証と保存を一致させる)。
func normalize(t time.Time) time.Time { return t.In(entity.JST).Truncate(time.Second) }

// validateTitle・validateCapacity は1項目ごとの値の規則(項目間の関係は validateRelations)。
func validateTitle(title string, errs *[]FieldError) {
	if n := utf8.RuneCountInString(title); n < 1 || n > titleMaxRunes {
		*errs = append(*errs, FieldError{"title", "1〜100文字で指定してください"})
	}
}

func validateCapacity(c int, errs *[]FieldError) {
	if c < capacityMin || c > capacityMax {
		*errs = append(*errs, FieldError{"capacity", "1〜1000で指定してください"})
	}
}

// validateRelations は日時の関係: startAt < endAt、reservationDeadlineAt <= startAt。
func validateRelations(s entity.Slot, errs *[]FieldError) {
	if !s.StartAt.Before(s.EndAt) {
		*errs = append(*errs, FieldError{"endAt", "startAt より後にしてください"})
	}
	if s.ReservationDeadlineAt.After(s.StartAt) {
		*errs = append(*errs, FieldError{"reservationDeadlineAt", "startAt 以前にしてください"})
	}
}

func toError(errs []FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Fields: errs}
}

// Create は枠を下書きとして作る(管理者)。
func (s *SlotService) Create(ctx context.Context, actor Actor, in CreateSlotInput) (entity.SlotWithRemaining, error) {
	if err := RequireAdmin(actor); err != nil {
		return entity.SlotWithRemaining{}, err
	}
	now := s.clock.Now()
	slot := entity.Slot{
		PublicID: s.newID(), Title: in.Title, Capacity: in.Capacity,
		StartAt: normalize(in.StartAt), EndAt: normalize(in.EndAt), ReservationDeadlineAt: normalize(in.ReservationDeadlineAt),
		Status: entity.SlotDraft, CreatedAt: now, UpdatedAt: now,
	}
	var errs []FieldError
	validateTitle(slot.Title, &errs)
	validateCapacity(slot.Capacity, &errs)
	validateRelations(slot, &errs)
	if err := toError(errs); err != nil {
		return entity.SlotWithRemaining{}, err
	}
	saved, err := s.slots.Insert(ctx, slot)
	if err != nil {
		return entity.SlotWithRemaining{}, err
	}
	return entity.SlotWithRemaining{Slot: saved, Remaining: saved.Capacity}, nil // 作成直後は予約が無い
}

// Get は枠の詳細を返す。利用者には下書きを見せない(404)。
func (s *SlotService) Get(ctx context.Context, actor Actor, publicID string) (entity.SlotWithRemaining, error) {
	slot, err := s.slots.GetWithRemaining(ctx, publicID)
	if err != nil {
		return entity.SlotWithRemaining{}, err
	}
	if !actor.IsAdmin && slot.Status == entity.SlotDraft {
		return entity.SlotWithRemaining{}, ErrNotFound
	}
	return slot, nil
}

// List は枠の一覧を返す。見える範囲の出し分けはここで行う(R4):
// 利用者は published かつ現在時刻 < 締切の枠だけ(status の指定は無視)、管理者は全状態(status で絞れる)。
func (s *SlotService) List(ctx context.Context, actor Actor, in ListSlotsInput) (SlotPage, error) {
	limit, offset := defaultLimit, 0
	var errs []FieldError
	if in.Limit != nil {
		limit = *in.Limit
		if limit < 1 || limit > maxLimit {
			errs = append(errs, FieldError{"limit", "1〜100で指定してください"})
		}
	}
	if in.Offset != nil {
		offset = *in.Offset
		if offset < 0 {
			errs = append(errs, FieldError{"offset", "0以上で指定してください"})
		}
	}
	if in.From != nil && in.To != nil && in.From.After(*in.To) {
		errs = append(errs, FieldError{"from", "to 以前にしてください"})
	}
	if actor.IsAdmin && in.Status != nil && !in.Status.Valid() {
		errs = append(errs, FieldError{"status", "draft・published・closed のどれかを指定してください"})
	}
	if err := toError(errs); err != nil {
		return SlotPage{}, err
	}

	q := SlotListQuery{From: in.From, To: in.To, AvailableOnly: in.AvailableOnly, Limit: limit, Offset: offset}
	if actor.IsAdmin {
		q.Status = in.Status
	} else {
		now := s.clock.Now()
		q.ReservableAt = &now
	}
	items, err := s.slots.List(ctx, q)
	if err != nil {
		return SlotPage{}, err
	}
	return SlotPage{Items: items, Limit: limit, Offset: offset}, nil
}

// Publish は下書きの枠を公開する(管理者)。
func (s *SlotService) Publish(ctx context.Context, actor Actor, publicID string) error {
	if err := RequireAdmin(actor); err != nil {
		return err
	}
	return s.tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.Slots().LockByPublicID(ctx, publicID) // 最初に枠の行をロックする
		if err != nil {
			return err
		}
		if locked.Slot.Status != entity.SlotDraft {
			return Business(CodeInvalidStateTransition, "下書きの枠だけ公開できる")
		}
		return tx.Slots().UpdateStatus(ctx, locked, entity.SlotPublished, s.clock.Now())
	})
}

// Close は公開中の枠を閉じる(管理者)。有効な予約がある枠は閉じられない。
func (s *SlotService) Close(ctx context.Context, actor Actor, publicID string) error {
	if err := RequireAdmin(actor); err != nil {
		return err
	}
	return s.tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.Slots().LockByPublicID(ctx, publicID)
		if err != nil {
			return err
		}
		if locked.Slot.Status != entity.SlotPublished {
			return Business(CodeInvalidStateTransition, "公開中の枠だけ閉じられる")
		}
		active, err := tx.Slots().HasActiveReservation(ctx, locked)
		if err != nil {
			return err
		}
		if active {
			return Business(CodeSlotHasActiveReservations, "有効な予約がある枠は閉じられない")
		}
		return tx.Slots().UpdateStatus(ctx, locked, entity.SlotClosed, s.clock.Now())
	})
}

// Delete は枠を物理削除する(管理者)。予約が一度も入っていない枠だけ(状態は問わない)。
func (s *SlotService) Delete(ctx context.Context, actor Actor, publicID string) error {
	if err := RequireAdmin(actor); err != nil {
		return err
	}
	return s.tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.Slots().LockByPublicID(ctx, publicID)
		if err != nil {
			return err
		}
		hasAny, err := tx.Slots().HasAnyReservation(ctx, locked) // R1: 取消済みも数える
		if err != nil {
			return err
		}
		if hasAny {
			return Business(CodeSlotHasReservationHistory, "予約が入ったことのある枠は削除できない")
		}
		return tx.Slots().Delete(ctx, locked)
	})
}

// Update は枠を変更する(管理者)。条件は R1〜R3:
//   - 閉じた枠は変更できない(invalid-state-transition)
//   - 予約が1行でもある(取消済みを含む)枠では、title・startAt・endAt を送れず、capacity・reservationDeadlineAt は
//     現在値以上にしかできない(update-not-allowed)。同値は無変更として通す。
//   - 変更後の値で項目間の関係を確かめる(違反は validation-error)。
func (s *SlotService) Update(ctx context.Context, actor Actor, publicID string, in UpdateSlotInput) error {
	if err := RequireAdmin(actor); err != nil {
		return err
	}
	// ロックの前にできる検証(項目単体の値)。
	var errs []FieldError
	if in.Title == nil && in.Capacity == nil && in.StartAt == nil && in.EndAt == nil && in.ReservationDeadlineAt == nil {
		errs = append(errs, FieldError{"", "変更する項目を1つ以上指定してください"})
	}
	if in.Title != nil {
		validateTitle(*in.Title, &errs)
	}
	if in.Capacity != nil {
		validateCapacity(*in.Capacity, &errs)
	}
	if err := toError(errs); err != nil {
		return err
	}

	return s.tx.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		locked, err := tx.Slots().LockByPublicID(ctx, publicID)
		if err != nil {
			return err
		}
		cur := locked.Slot
		if cur.Status == entity.SlotClosed {
			return Business(CodeInvalidStateTransition, "閉じた枠は変更できない")
		}
		hasAny, err := tx.Slots().HasAnyReservation(ctx, locked)
		if err != nil {
			return err
		}
		if hasAny {
			const msg = "予約が入った枠では、定員の増加と締切の延長だけ変更できる"
			if in.Title != nil || in.StartAt != nil || in.EndAt != nil {
				return Business(CodeUpdateNotAllowed, msg)
			}
			if in.Capacity != nil && *in.Capacity < cur.Capacity {
				return Business(CodeUpdateNotAllowed, msg)
			}
			if in.ReservationDeadlineAt != nil && normalize(*in.ReservationDeadlineAt).Before(cur.ReservationDeadlineAt) {
				return Business(CodeUpdateNotAllowed, msg)
			}
		}

		next := cur
		if in.Title != nil {
			next.Title = *in.Title
		}
		if in.Capacity != nil {
			next.Capacity = *in.Capacity
		}
		if in.StartAt != nil {
			next.StartAt = normalize(*in.StartAt)
		}
		if in.EndAt != nil {
			next.EndAt = normalize(*in.EndAt)
		}
		if in.ReservationDeadlineAt != nil {
			next.ReservationDeadlineAt = normalize(*in.ReservationDeadlineAt)
		}
		var relErrs []FieldError
		validateRelations(next, &relErrs)
		if err := toError(relErrs); err != nil {
			return err
		}
		next.UpdatedAt = s.clock.Now()
		return tx.Slots().UpdateFields(ctx, locked, next)
	})
}
