package usecase_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// ---- 試験の道具 -----------------------------------------------------------

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// now は固定の現在時刻(JST)。
var now = time.Date(2026, 10, 25, 12, 0, 0, 0, entity.JST)

// fakeSlots は SlotStore・SlotTx・Tx・TxRunner を1つで兼ねる偽物。
// 呼び出しを calls に順に残すので、「認可は DB より前」「最初に lock」を断言できる。
type fakeSlots struct {
	slot      entity.Slot // Lock と Get が返す枠
	remaining int         // Get が返す残席
	hasAny    bool        // HasAnyReservation の答え(取消済みを含む)
	hasActive bool        // HasActiveReservation の答え

	lockErr   error // 非 nil なら Lock が返す
	hasAnyErr error // 非 nil なら HasAnyReservation が返す
	activeErr error // 非 nil なら HasActiveReservation が返す
	getErr    error
	insertErr error
	listErr   error

	listItems []entity.SlotWithRemaining

	calls []string

	inserted   entity.Slot
	listQuery  usecase.SlotListQuery
	updated    entity.Slot
	statusTo   entity.SlotStatus
	statusAt   time.Time
	lockedWith string
}

func (f *fakeSlots) Insert(_ context.Context, s entity.Slot) (entity.Slot, error) {
	f.calls = append(f.calls, "insert")
	f.inserted = s
	if f.insertErr != nil {
		return entity.Slot{}, f.insertErr
	}
	s.ID = 7
	return s, nil
}

func (f *fakeSlots) GetWithRemaining(_ context.Context, _ string) (entity.SlotWithRemaining, error) {
	f.calls = append(f.calls, "get")
	if f.getErr != nil {
		return entity.SlotWithRemaining{}, f.getErr
	}
	return entity.SlotWithRemaining{Slot: f.slot, Remaining: f.remaining}, nil
}

func (f *fakeSlots) List(_ context.Context, q usecase.SlotListQuery) ([]entity.SlotWithRemaining, error) {
	f.calls = append(f.calls, "list")
	f.listQuery = q
	return f.listItems, f.listErr
}

func (f *fakeSlots) LockByPublicID(_ context.Context, id string) (usecase.LockedSlot, error) {
	f.calls = append(f.calls, "lock")
	f.lockedWith = id
	if f.lockErr != nil {
		return usecase.LockedSlot{}, f.lockErr
	}
	return usecase.LockedSlot{Slot: f.slot}, nil
}

func (f *fakeSlots) HasAnyReservation(_ context.Context, _ usecase.LockedSlot) (bool, error) {
	f.calls = append(f.calls, "any")
	return f.hasAny, f.hasAnyErr
}

func (f *fakeSlots) HasActiveReservation(_ context.Context, _ usecase.LockedSlot) (bool, error) {
	f.calls = append(f.calls, "active")
	return f.hasActive, f.activeErr
}

func (f *fakeSlots) UpdateFields(_ context.Context, _ usecase.LockedSlot, to entity.Slot) error {
	f.calls = append(f.calls, "update")
	f.updated = to
	return nil
}

func (f *fakeSlots) UpdateStatus(_ context.Context, _ usecase.LockedSlot, to entity.SlotStatus, at time.Time) error {
	f.calls = append(f.calls, "status:"+string(to))
	f.statusTo = to
	f.statusAt = at
	return nil
}

func (f *fakeSlots) Delete(_ context.Context, _ usecase.LockedSlot) error {
	f.calls = append(f.calls, "delete")
	return nil
}

func (f *fakeSlots) Slots() usecase.SlotTx { return f }

func (f *fakeSlots) WithinTx(ctx context.Context, fn func(context.Context, usecase.Tx) error) error {
	f.calls = append(f.calls, "begin")
	return fn(ctx, f)
}

var (
	adminActor = usecase.Actor{UserID: 1, Sub: "admin-sub", IsAdmin: true}
	userActor  = usecase.Actor{UserID: 2, Sub: "user-sub"}
)

func newSvc(f *fakeSlots) *usecase.SlotService {
	return usecase.NewSlotService(f, f, fixedClock{now}, func() string { return "new-public-id" })
}

func ptr[T any](v T) *T { return &v }

// slotStart は基準の枠の開始(2026-11-01 09:00 JST)。終了は +1h、締切は -1h。
var slotStart = time.Date(2026, 11, 1, 9, 0, 0, 0, entity.JST)

func baseSlot(st entity.SlotStatus) entity.Slot {
	return entity.Slot{
		ID: 1, PublicID: "p", Title: "title", Capacity: 2,
		StartAt: slotStart, EndAt: slotStart.Add(time.Hour), ReservationDeadlineAt: slotStart.Add(-time.Hour),
		Status:    st,
		CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour),
	}
}

func validCreate() usecase.CreateSlotInput {
	return usecase.CreateSlotInput{
		Title: "title", Capacity: 2,
		StartAt: slotStart, EndAt: slotStart.Add(time.Hour), ReservationDeadlineAt: slotStart.Add(-time.Hour),
	}
}

// wantCalls は呼び出しの並びが want と同じことを確かめる。
func wantCalls(t *testing.T, f *fakeSlots, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := f.calls
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// wantBusiness は err が業務エラー code であること。
func wantBusiness(t *testing.T, err error, code usecase.Code) {
	t.Helper()
	if !usecase.IsBusiness(err, code) {
		t.Errorf("err = %v, want 業務エラー %s", err, code)
	}
}

// wantFields は err が ValidationError で、誤りの項目名がこの並びであること。
func wantFields(t *testing.T, err error, fields ...string) {
	t.Helper()
	var ve *usecase.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	var got []string
	for _, fe := range ve.Fields {
		got = append(got, fe.Field)
		if fe.Message == "" {
			t.Errorf("項目 %q のメッセージが空", fe.Field)
		}
	}
	if !reflect.DeepEqual(got, fields) {
		t.Errorf("誤りの項目 = %v, want %v", got, fields)
	}
}

// ---- 認可(R11: 403 は DB より前) -------------------------------------------

// 管理者専用の操作は、利用者・未認証(ゼロ値)なら ErrForbidden で、DB に一切触れない。
// 本文が不正でも 400 でなく 403 が先(R11)。
func TestAdminOnly_ForbiddenBeforeAnyDBCall(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(*usecase.SlotService, usecase.Actor) error{
		"Create": func(s *usecase.SlotService, a usecase.Actor) error {
			_, err := s.Create(ctx, a, usecase.CreateSlotInput{}) // 空の入力=本来は 400
			return err
		},
		"Update": func(s *usecase.SlotService, a usecase.Actor) error {
			return s.Update(ctx, a, "p", usecase.UpdateSlotInput{}) // 空の変更=本来は 400
		},
		"Publish": func(s *usecase.SlotService, a usecase.Actor) error { return s.Publish(ctx, a, "p") },
		"Close":   func(s *usecase.SlotService, a usecase.Actor) error { return s.Close(ctx, a, "p") },
		"Delete":  func(s *usecase.SlotService, a usecase.Actor) error { return s.Delete(ctx, a, "p") },
	}
	actors := map[string]usecase.Actor{"利用者": userActor, "ゼロ値": {}}
	for opName, op := range ops {
		for actorName, actor := range actors {
			t.Run(opName+"/"+actorName, func(t *testing.T) {
				f := &fakeSlots{slot: baseSlot(entity.SlotPublished)}
				if err := op(newSvc(f), actor); !errors.Is(err, usecase.ErrForbidden) {
					t.Errorf("err = %v, want ErrForbidden", err)
				}
				wantCalls(t, f) // begin も lock も無い
			})
		}
	}
}

// ---- ロックが先(D-3・仕様「同時実行」) ---------------------------------------

// 更新系は、成功でも各種の失敗でも、トランザクションの最初の呼び出しが lock。
// 予約の有無(any/active)を数えるのはその後。
func TestUpdateOps_LockIsFirstCall(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		f    func() *fakeSlots
		op   func(*usecase.SlotService) error
	}{
		{"Publish 成功", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotDraft)} },
			func(s *usecase.SlotService) error { return s.Publish(ctx, adminActor, "p") }},
		{"Publish 状態違い", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotPublished)} },
			func(s *usecase.SlotService) error { return s.Publish(ctx, adminActor, "p") }},
		{"Close 成功", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotPublished)} },
			func(s *usecase.SlotService) error { return s.Close(ctx, adminActor, "p") }},
		{"Close 有効な予約あり", func() *fakeSlots {
			return &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true, hasActive: true}
		}, func(s *usecase.SlotService) error { return s.Close(ctx, adminActor, "p") }},
		{"Delete 成功", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotDraft)} },
			func(s *usecase.SlotService) error { return s.Delete(ctx, adminActor, "p") }},
		{"Delete 予約履歴あり", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotDraft), hasAny: true} },
			func(s *usecase.SlotService) error { return s.Delete(ctx, adminActor, "p") }},
		{"Update 成功", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotDraft)} },
			func(s *usecase.SlotService) error {
				return s.Update(ctx, adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
			}},
		{"Update 予約ありで不可", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true} },
			func(s *usecase.SlotService) error {
				return s.Update(ctx, adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
			}},
		{"Update 項目間の違反", func() *fakeSlots { return &fakeSlots{slot: baseSlot(entity.SlotDraft)} },
			func(s *usecase.SlotService) error {
				return s.Update(ctx, adminActor, "p", usecase.UpdateSlotInput{EndAt: ptr(slotStart.Add(-time.Hour))})
			}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f()
			_ = tt.op(newSvc(f))
			if len(f.calls) < 2 || f.calls[0] != "begin" || f.calls[1] != "lock" {
				t.Errorf("calls = %v, want begin → lock で始まる", f.calls)
			}
			if f.lockedWith != "p" {
				t.Errorf("lock に渡した ID = %q, want p", f.lockedWith)
			}
		})
	}
}

// 枠が無い(ロックで ErrNotFound)なら、404 のまま返し、後続の呼び出しをしない。
func TestUpdateOps_NotFoundAtLock(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(*usecase.SlotService) error{
		"Publish": func(s *usecase.SlotService) error { return s.Publish(ctx, adminActor, "p") },
		"Close":   func(s *usecase.SlotService) error { return s.Close(ctx, adminActor, "p") },
		"Delete":  func(s *usecase.SlotService) error { return s.Delete(ctx, adminActor, "p") },
		"Update": func(s *usecase.SlotService) error {
			return s.Update(ctx, adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			f := &fakeSlots{lockErr: usecase.ErrNotFound}
			if err := op(newSvc(f)); !errors.Is(err, usecase.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
			wantCalls(t, f, "begin", "lock")
		})
	}
}

// DB の障害は業務エラーに化けず、そのまま(handler が 500 にする)返る。
func TestUpdate_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAnyErr: boom}
	err := newSvc(f).Update(context.Background(), adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	wantCalls(t, f, "begin", "lock", "any")
}

// Close・Delete でも、予約の有無を引く DB の障害は業務エラーに化けずに返る。
func TestCloseDelete_StoreErrorPropagates(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	t.Run("Close", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished), activeErr: boom}
		if err := newSvc(f).Close(ctx, adminActor, "p"); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
		wantCalls(t, f, "begin", "lock", "active") // 書き込まない
	})
	t.Run("Delete", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotDraft), hasAnyErr: boom}
		if err := newSvc(f).Delete(ctx, adminActor, "p"); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
		wantCalls(t, f, "begin", "lock", "any") // 消さない
	})
}

// ---- Publish ------------------------------------------------------------

func TestPublish(t *testing.T) {
	ctx := context.Background()
	t.Run("draft は published になり、時刻は Clock の now", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotDraft)}
		if err := newSvc(f).Publish(ctx, adminActor, "p"); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, f, "begin", "lock", "status:published")
		if !f.statusAt.Equal(now) {
			t.Errorf("updated_at = %v, want %v", f.statusAt, now)
		}
	})
	for _, st := range []entity.SlotStatus{entity.SlotPublished, entity.SlotClosed} {
		t.Run(string(st)+" からは 422 invalid-state-transition", func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st)}
			wantBusiness(t, newSvc(f).Publish(ctx, adminActor, "p"), usecase.CodeInvalidStateTransition)
			wantCalls(t, f, "begin", "lock") // 書き込まない
		})
	}
}

// ---- Close --------------------------------------------------------------

func TestClose(t *testing.T) {
	ctx := context.Background()
	t.Run("published で有効な予約なしなら closed", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished)}
		if err := newSvc(f).Close(ctx, adminActor, "p"); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, f, "begin", "lock", "active", "status:closed")
		if !f.statusAt.Equal(now) {
			t.Errorf("updated_at = %v, want %v", f.statusAt, now)
		}
	})
	t.Run("有効な予約があれば 409 slot-has-active-reservations", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true, hasActive: true}
		wantBusiness(t, newSvc(f).Close(ctx, adminActor, "p"), usecase.CodeSlotHasActiveReservations)
		wantCalls(t, f, "begin", "lock", "active") // 書き込まない
	})
	t.Run("取消済みの予約だけなら閉じられる(閉じる判定は active のみ)", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true, hasActive: false}
		if err := newSvc(f).Close(ctx, adminActor, "p"); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, f, "begin", "lock", "active", "status:closed")
	})
	for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotClosed} {
		t.Run(string(st)+" からは 422 invalid-state-transition(予約の有無は見ない)", func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st), hasActive: true}
			wantBusiness(t, newSvc(f).Close(ctx, adminActor, "p"), usecase.CodeInvalidStateTransition)
			wantCalls(t, f, "begin", "lock")
		})
	}
}

// ---- Delete(R1: 取消済みも「予約が入っている」) --------------------------------

func TestDelete(t *testing.T) {
	ctx := context.Background()
	for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotPublished, entity.SlotClosed} {
		t.Run(string(st)+" でも予約が一度も無ければ削除できる", func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st)}
			if err := newSvc(f).Delete(ctx, adminActor, "p"); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, f, "begin", "lock", "any", "delete")
		})
		t.Run(string(st)+" で予約履歴があれば 422 slot-has-reservation-history", func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st), hasAny: true}
			wantBusiness(t, newSvc(f).Delete(ctx, adminActor, "p"), usecase.CodeSlotHasReservationHistory)
			wantCalls(t, f, "begin", "lock", "any") // 消さない
		})
	}
	t.Run("取消済みの予約だけでも削除できない(active が 0 でも any で判定する)", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotClosed), hasAny: true, hasActive: false}
		wantBusiness(t, newSvc(f).Delete(ctx, adminActor, "p"), usecase.CodeSlotHasReservationHistory)
		for _, c := range f.calls {
			if c == "active" || c == "delete" {
				t.Errorf("calls = %v: Delete は active を見ず、消さない", f.calls)
			}
		}
	})
}

// ---- Update(R1〜R3) -----------------------------------------------------

func TestUpdate_Validation_BeforeLock(t *testing.T) {
	ctx := context.Background()
	runes := func(n int, r string) *string { return ptr(strings.Repeat(r, n)) }
	cases := []struct {
		name   string
		in     usecase.UpdateSlotInput
		fields []string // 空なら通る
	}{
		{"何も指定しない", usecase.UpdateSlotInput{}, []string{""}},
		{"title 0 文字", usecase.UpdateSlotInput{Title: ptr("")}, []string{"title"}},
		{"title 1 文字", usecase.UpdateSlotInput{Title: runes(1, "a")}, nil},
		{"title 100 文字", usecase.UpdateSlotInput{Title: runes(100, "a")}, nil},
		{"title 101 文字", usecase.UpdateSlotInput{Title: runes(101, "a")}, []string{"title"}},
		{"title 100 文字(日本語。バイトでなく rune で数える)", usecase.UpdateSlotInput{Title: runes(100, "あ")}, nil},
		{"title 101 文字(日本語)", usecase.UpdateSlotInput{Title: runes(101, "あ")}, []string{"title"}},
		{"title 100 文字(絵文字)", usecase.UpdateSlotInput{Title: runes(100, "😀")}, nil},
		{"title 101 文字(絵文字)", usecase.UpdateSlotInput{Title: runes(101, "😀")}, []string{"title"}},
		{"capacity 0", usecase.UpdateSlotInput{Capacity: ptr(0)}, []string{"capacity"}},
		{"capacity 1", usecase.UpdateSlotInput{Capacity: ptr(1)}, nil},
		{"capacity 1000", usecase.UpdateSlotInput{Capacity: ptr(1000)}, nil},
		{"capacity 1001", usecase.UpdateSlotInput{Capacity: ptr(1001)}, []string{"capacity"}},
		{"capacity 負", usecase.UpdateSlotInput{Capacity: ptr(-1)}, []string{"capacity"}},
		{"title と capacity が両方不正", usecase.UpdateSlotInput{Title: ptr(""), Capacity: ptr(0)}, []string{"title", "capacity"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(entity.SlotDraft)}
			err := newSvc(f).Update(ctx, adminActor, "p", tt.in)
			if tt.fields == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			wantFields(t, err, tt.fields...)
			wantCalls(t, f) // 単体の値の誤りはロックの前に返す(DB に触れない)
		})
	}
}

func TestUpdate_ClosedSlot_422(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotClosed)}
	err := newSvc(f).Update(context.Background(), adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
	wantBusiness(t, err, usecase.CodeInvalidStateTransition)
	wantCalls(t, f, "begin", "lock") // 予約の有無を見る前に返る
}

func TestUpdate_NoReservation_AllFieldsChangeable(t *testing.T) {
	for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotPublished} {
		t.Run(string(st), func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st)}
			newStart := slotStart.Add(24 * time.Hour)
			in := usecase.UpdateSlotInput{
				Title:                 ptr("new title"),
				Capacity:              ptr(1), // 予約が無ければ減らせる
				StartAt:               ptr(newStart),
				EndAt:                 ptr(newStart.Add(2 * time.Hour)),
				ReservationDeadlineAt: ptr(newStart.Add(-30 * time.Minute)),
			}
			if err := newSvc(f).Update(context.Background(), adminActor, "p", in); err != nil {
				t.Fatal(err)
			}
			wantCalls(t, f, "begin", "lock", "any", "update")
			u := f.updated
			if u.Title != "new title" || u.Capacity != 1 ||
				!u.StartAt.Equal(newStart) || !u.EndAt.Equal(newStart.Add(2*time.Hour)) ||
				!u.ReservationDeadlineAt.Equal(newStart.Add(-30*time.Minute)) {
				t.Errorf("updated = %+v", u)
			}
			if u.Status != st || u.ID != 1 || u.PublicID != "p" {
				t.Errorf("状態・ID は変わらない: %+v", u)
			}
			if !u.UpdatedAt.Equal(now) || !u.CreatedAt.Equal(f.slot.CreatedAt) {
				t.Errorf("updated_at = %v(now のはず), created_at = %v(据え置きのはず)", u.UpdatedAt, u.CreatedAt)
			}
		})
	}
}

// 本文に無い項目は現在値のまま(部分更新)。
func TestUpdate_PartialKeepsOtherFields(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotDraft)}
	if err := newSvc(f).Update(context.Background(), adminActor, "p", usecase.UpdateSlotInput{Title: ptr("only title")}); err != nil {
		t.Fatal(err)
	}
	want := f.slot
	want.Title = "only title"
	want.UpdatedAt = now
	if !reflect.DeepEqual(f.updated, want) {
		t.Errorf("updated = %+v\nwant     %+v", f.updated, want)
	}
}

// R2: 同値の PATCH は無変更として通る(予約があっても capacity・締切は同値なら可)。
func TestUpdate_SameValuePasses(t *testing.T) {
	t.Run("予約なし: 全項目を同値で", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished)}
		s := f.slot
		in := usecase.UpdateSlotInput{Title: &s.Title, Capacity: &s.Capacity, StartAt: &s.StartAt, EndAt: &s.EndAt, ReservationDeadlineAt: &s.ReservationDeadlineAt}
		if err := newSvc(f).Update(context.Background(), adminActor, "p", in); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, f, "begin", "lock", "any", "update")
	})
	t.Run("予約あり: capacity と締切を同値で", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true}
		s := f.slot
		in := usecase.UpdateSlotInput{Capacity: &s.Capacity, ReservationDeadlineAt: &s.ReservationDeadlineAt}
		if err := newSvc(f).Update(context.Background(), adminActor, "p", in); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, f, "begin", "lock", "any", "update")
	})
}

// R1・R2: 予約が1行でもあれば(取消済みだけでも hasAny=true)制限がかかる。
func TestUpdate_WithReservations_Restricted(t *testing.T) {
	ctx := context.Background()
	for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotPublished} { // 状態を問わない
		base := baseSlot(st)
		cases := []struct {
			name string
			in   usecase.UpdateSlotInput
			ok   bool
		}{
			// title・startAt・endAt は本文にあるだけで不可(同値でも)。
			{"title", usecase.UpdateSlotInput{Title: ptr("x")}, false},
			{"title(同値でも)", usecase.UpdateSlotInput{Title: ptr(base.Title)}, false},
			{"startAt", usecase.UpdateSlotInput{StartAt: ptr(base.StartAt.Add(time.Minute))}, false},
			{"startAt(同値でも)", usecase.UpdateSlotInput{StartAt: ptr(base.StartAt)}, false},
			{"endAt", usecase.UpdateSlotInput{EndAt: ptr(base.EndAt.Add(time.Minute))}, false},
			{"endAt(同値でも)", usecase.UpdateSlotInput{EndAt: ptr(base.EndAt)}, false},
			{"許された項目と一緒でも title があれば不可", usecase.UpdateSlotInput{Title: ptr("x"), Capacity: ptr(3)}, false},
			// capacity は増やすのと同値だけ。
			{"capacity 増", usecase.UpdateSlotInput{Capacity: ptr(3)}, true},
			{"capacity 同値", usecase.UpdateSlotInput{Capacity: ptr(2)}, true},
			{"capacity 減", usecase.UpdateSlotInput{Capacity: ptr(1)}, false},
			// 締切は延ばすのと同値だけ。
			{"締切 延長", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(time.Minute))}, true},
			{"締切 同値", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt)}, true},
			{"締切 短縮", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(-time.Minute))}, false},
			// 秒未満は切り捨ててから比べる(同値の締切に小数秒が付いても短縮扱いにしない)。
			{"締切 同値+小数秒", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(900 * time.Millisecond))}, true},
			{"締切 -1秒+小数秒 は切り捨てで短縮", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(-time.Second + 900*time.Millisecond))}, false},
			// UTC で同じ瞬間を送っても同値。
			{"締切 同値(UTC表記)", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.UTC())}, true},
			{"capacity と締切を両方延ばす", usecase.UpdateSlotInput{Capacity: ptr(10), ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(time.Minute))}, true},
			{"capacity は増、締切は短縮 → 不可", usecase.UpdateSlotInput{Capacity: ptr(10), ReservationDeadlineAt: ptr(base.ReservationDeadlineAt.Add(-time.Minute))}, false},
		}
		for _, tt := range cases {
			t.Run(string(st)+"/"+tt.name, func(t *testing.T) {
				f := &fakeSlots{slot: base, hasAny: true}
				err := newSvc(f).Update(ctx, adminActor, "p", tt.in)
				if tt.ok {
					if err != nil {
						t.Fatalf("err = %v, want nil", err)
					}
					wantCalls(t, f, "begin", "lock", "any", "update")
					return
				}
				wantBusiness(t, err, usecase.CodeUpdateNotAllowed)
				wantCalls(t, f, "begin", "lock", "any") // 書き込まない
			})
		}
	}
}

// R1: 予約が取消済みだけ(hasActive=false, hasAny=true)でも、変更の制限は「予約あり」と同じ。
func TestUpdate_CancelledOnlySlot_StillRestricted(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true, hasActive: false}
	err := newSvc(f).Update(context.Background(), adminActor, "p", usecase.UpdateSlotInput{Title: ptr("x")})
	wantBusiness(t, err, usecase.CodeUpdateNotAllowed)
	for _, c := range f.calls {
		if c == "active" {
			t.Errorf("calls = %v: Update は active でなく any で判定する(R1)", f.calls)
		}
	}
}

// R3: 現在値に本文を重ねた後の値で項目間の関係を確かめる。違反は 400(ロックの後)。
func TestUpdate_RelationsCheckedAfterMerge(t *testing.T) {
	ctx := context.Background()
	s := baseSlot(entity.SlotDraft) // start 09:00, end 10:00, deadline 08:00
	cases := []struct {
		name   string
		in     usecase.UpdateSlotInput
		fields []string // 空なら通る
	}{
		{"endAt だけ start 以前に(現在の start と比べる)", usecase.UpdateSlotInput{EndAt: ptr(s.StartAt.Add(-time.Minute))}, []string{"endAt"}},
		{"endAt = start", usecase.UpdateSlotInput{EndAt: ptr(s.StartAt)}, []string{"endAt"}},
		{"endAt = start + 1秒", usecase.UpdateSlotInput{EndAt: ptr(s.StartAt.Add(time.Second))}, nil},
		{"startAt だけ end 以後に(現在の end と比べる)", usecase.UpdateSlotInput{StartAt: ptr(s.EndAt.Add(time.Minute))}, []string{"endAt"}},
		{"startAt = end", usecase.UpdateSlotInput{StartAt: ptr(s.EndAt)}, []string{"endAt"}},
		{"startAt だけ締切より前に(現在の締切と比べる)", usecase.UpdateSlotInput{StartAt: ptr(s.ReservationDeadlineAt.Add(-time.Minute))}, []string{"reservationDeadlineAt"}},
		{"startAt = 締切(可)", usecase.UpdateSlotInput{StartAt: ptr(s.ReservationDeadlineAt)}, nil},
		{"締切だけ start より後に", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(s.StartAt.Add(time.Minute))}, []string{"reservationDeadlineAt"}},
		{"締切 = start(可)", usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(s.StartAt)}, nil},
		{"全部そろえて新しい日時へ(可)", usecase.UpdateSlotInput{
			StartAt: ptr(s.StartAt.Add(48 * time.Hour)), EndAt: ptr(s.EndAt.Add(48 * time.Hour)), ReservationDeadlineAt: ptr(s.StartAt.Add(47 * time.Hour))}, nil},
		{"start と end を逆にして同時に送る", usecase.UpdateSlotInput{StartAt: ptr(s.EndAt), EndAt: ptr(s.StartAt)}, []string{"endAt"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSlots{slot: s}
			err := newSvc(f).Update(ctx, adminActor, "p", tt.in)
			if tt.fields == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				wantCalls(t, f, "begin", "lock", "any", "update")
				return
			}
			var ve *usecase.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			// 項目名の並びの比較は、endAt/reservationDeadlineAt の2つが同時に出る場合も含めて緩く: 先頭が期待どおり。
			if ve.Fields[0].Field != tt.fields[0] {
				t.Errorf("先頭の誤り = %q, want %q (all=%v)", ve.Fields[0].Field, tt.fields[0], ve.Fields)
			}
			wantCalls(t, f, "begin", "lock", "any") // 400 のときは書かない。かつ 404 の判定(lock)の後
		})
	}
}

// R2 の 422 は R3 の 400 より先に返る(予約ありで title を送りつつ関係も壊れていれば 422)。
func TestUpdate_NotAllowedBeforeRelationCheck(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true}
	in := usecase.UpdateSlotInput{Title: ptr("x"), EndAt: ptr(slotStart.Add(-time.Hour))}
	wantBusiness(t, newSvc(f).Update(context.Background(), adminActor, "p", in), usecase.CodeUpdateNotAllowed)
}

// 予約ありで締切を延ばした結果が start を超えるなら、R3 の 400。
func TestUpdate_WithReservations_DeadlineBeyondStart_400(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotPublished), hasAny: true}
	in := usecase.UpdateSlotInput{ReservationDeadlineAt: ptr(slotStart.Add(time.Minute))}
	wantFields(t, newSvc(f).Update(context.Background(), adminActor, "p", in), "reservationDeadlineAt")
	wantCalls(t, f, "begin", "lock", "any")
}

// R12: 秒未満は切り捨て、JST で保存する。
func TestUpdate_NormalizesTimes(t *testing.T) {
	f := &fakeSlots{slot: baseSlot(entity.SlotDraft)}
	utcStart := time.Date(2026, 12, 1, 1, 0, 0, 900_000_000, time.UTC) // JST 10:00:00.9
	in := usecase.UpdateSlotInput{
		StartAt:               ptr(utcStart),
		EndAt:                 ptr(utcStart.Add(time.Hour - 400*time.Millisecond)), // JST 11:00:00.5
		ReservationDeadlineAt: ptr(utcStart),
	}
	if err := newSvc(f).Update(context.Background(), adminActor, "p", in); err != nil {
		t.Fatal(err)
	}
	u := f.updated
	wantStart := time.Date(2026, 12, 1, 10, 0, 0, 0, entity.JST)
	if !u.StartAt.Equal(wantStart) || u.StartAt.Nanosecond() != 0 || u.StartAt.Location() != entity.JST {
		t.Errorf("startAt = %v (%v), want %v JST・秒未満なし", u.StartAt, u.StartAt.Location(), wantStart)
	}
	if !u.EndAt.Equal(wantStart.Add(time.Hour)) || u.EndAt.Nanosecond() != 0 {
		t.Errorf("endAt = %v", u.EndAt)
	}
	if !u.ReservationDeadlineAt.Equal(wantStart) || u.ReservationDeadlineAt.Nanosecond() != 0 {
		t.Errorf("deadline = %v", u.ReservationDeadlineAt)
	}
}

// ---- Create -------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	f := &fakeSlots{}
	in := validCreate()
	got, err := newSvc(f).Create(context.Background(), adminActor, in)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls(t, f, "insert") // トランザクション・ロックは使わない
	ins := f.inserted
	if ins.PublicID != "new-public-id" || ins.Status != entity.SlotDraft {
		t.Errorf("inserted = %+v: 採番した ID・draft のはず", ins)
	}
	if !ins.CreatedAt.Equal(now) || !ins.UpdatedAt.Equal(now) {
		t.Errorf("created_at = %v, updated_at = %v, want now", ins.CreatedAt, ins.UpdatedAt)
	}
	if ins.Title != in.Title || ins.Capacity != in.Capacity || !ins.StartAt.Equal(in.StartAt) ||
		!ins.EndAt.Equal(in.EndAt) || !ins.ReservationDeadlineAt.Equal(in.ReservationDeadlineAt) {
		t.Errorf("inserted = %+v, 入力どおりのはず", ins)
	}
	if got.ID != 7 {
		t.Errorf("ID = %d: Insert の戻り値(ID 入り)を返すはず", got.ID)
	}
	if got.Remaining != in.Capacity {
		t.Errorf("remaining = %d, want %d(作成直後は予約が無い)", got.Remaining, in.Capacity)
	}
}

func TestCreate_Validation(t *testing.T) {
	ctx := context.Background()
	with := func(mod func(*usecase.CreateSlotInput)) usecase.CreateSlotInput {
		in := validCreate()
		mod(&in)
		return in
	}
	runes := func(n int, r string) string { return strings.Repeat(r, n) }
	cases := []struct {
		name   string
		in     usecase.CreateSlotInput
		fields []string // 空なら通る
	}{
		{"正しい入力", validCreate(), nil},
		{"title 0 文字", with(func(in *usecase.CreateSlotInput) { in.Title = "" }), []string{"title"}},
		{"title 1 文字", with(func(in *usecase.CreateSlotInput) { in.Title = "a" }), nil},
		{"title 100 文字", with(func(in *usecase.CreateSlotInput) { in.Title = runes(100, "a") }), nil},
		{"title 101 文字", with(func(in *usecase.CreateSlotInput) { in.Title = runes(101, "a") }), []string{"title"}},
		{"title 100 文字(日本語)", with(func(in *usecase.CreateSlotInput) { in.Title = runes(100, "あ") }), nil},
		{"title 101 文字(日本語)", with(func(in *usecase.CreateSlotInput) { in.Title = runes(101, "あ") }), []string{"title"}},
		{"title 100 文字(絵文字)", with(func(in *usecase.CreateSlotInput) { in.Title = runes(100, "😀") }), nil},
		{"title 101 文字(絵文字)", with(func(in *usecase.CreateSlotInput) { in.Title = runes(101, "😀") }), []string{"title"}},
		{"capacity 0", with(func(in *usecase.CreateSlotInput) { in.Capacity = 0 }), []string{"capacity"}},
		{"capacity 1", with(func(in *usecase.CreateSlotInput) { in.Capacity = 1 }), nil},
		{"capacity 1000", with(func(in *usecase.CreateSlotInput) { in.Capacity = 1000 }), nil},
		{"capacity 1001", with(func(in *usecase.CreateSlotInput) { in.Capacity = 1001 }), []string{"capacity"}},
		{"start = end", with(func(in *usecase.CreateSlotInput) { in.EndAt = in.StartAt }), []string{"endAt"}},
		{"start > end", with(func(in *usecase.CreateSlotInput) { in.EndAt = in.StartAt.Add(-time.Minute) }), []string{"endAt"}},
		{"start + 1秒 = end", with(func(in *usecase.CreateSlotInput) { in.EndAt = in.StartAt.Add(time.Second) }), nil},
		{"deadline = start(可)", with(func(in *usecase.CreateSlotInput) { in.ReservationDeadlineAt = in.StartAt }), nil},
		{"deadline > start", with(func(in *usecase.CreateSlotInput) { in.ReservationDeadlineAt = in.StartAt.Add(time.Second) }), []string{"reservationDeadlineAt"}},
		{"過去の日時も通す(R12: 過去の検査は足さない)", with(func(in *usecase.CreateSlotInput) {
			in.StartAt = now.Add(-72 * time.Hour)
			in.EndAt = now.Add(-71 * time.Hour)
			in.ReservationDeadlineAt = now.Add(-73 * time.Hour)
		}), nil},
		{"全部不正なら全部の項目を返す", usecase.CreateSlotInput{Title: "", Capacity: 0, StartAt: slotStart, EndAt: slotStart, ReservationDeadlineAt: slotStart.Add(time.Hour)},
			[]string{"title", "capacity", "endAt", "reservationDeadlineAt"}},
		// R12: 小数秒は切り捨ててから検証する。
		{"start 10:00:00.9 / end 10:00:00.1 は切り捨てで同時刻 → 不可", with(func(in *usecase.CreateSlotInput) {
			in.StartAt = time.Date(2026, 11, 1, 10, 0, 0, 900_000_000, entity.JST)
			in.EndAt = time.Date(2026, 11, 1, 10, 0, 0, 100_000_000, entity.JST)
			in.ReservationDeadlineAt = time.Date(2026, 11, 1, 9, 0, 0, 0, entity.JST)
		}), []string{"endAt"}},
		{"deadline 10:00:00.9 / start 10:00:00.1 は切り捨てで同時刻 → 可", with(func(in *usecase.CreateSlotInput) {
			in.StartAt = time.Date(2026, 11, 1, 10, 0, 0, 100_000_000, entity.JST)
			in.EndAt = time.Date(2026, 11, 1, 11, 0, 0, 0, entity.JST)
			in.ReservationDeadlineAt = time.Date(2026, 11, 1, 10, 0, 0, 900_000_000, entity.JST)
		}), nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeSlots{}
			_, err := newSvc(f).Create(ctx, adminActor, tt.in)
			if tt.fields == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				wantCalls(t, f, "insert")
				return
			}
			wantFields(t, err, tt.fields...)
			wantCalls(t, f) // 検証に落ちたら DB に触れない
		})
	}
}

// R12: JST に直し、秒未満を切り捨てて保存する。
func TestCreate_NormalizesTimes(t *testing.T) {
	f := &fakeSlots{}
	in := usecase.CreateSlotInput{
		Title: "t", Capacity: 1,
		StartAt:               time.Date(2026, 12, 1, 1, 0, 0, 900_000_000, time.UTC), // JST 10:00:00.9
		EndAt:                 time.Date(2026, 12, 1, 2, 0, 0, 999_999_999, time.UTC),
		ReservationDeadlineAt: time.Date(2026, 11, 30, 15, 0, 0, 0, time.UTC), // JST 12/1 00:00
	}
	if _, err := newSvc(f).Create(context.Background(), adminActor, in); err != nil {
		t.Fatal(err)
	}
	ins := f.inserted
	cases := []struct {
		name string
		got  time.Time
		want time.Time
	}{
		{"startAt", ins.StartAt, time.Date(2026, 12, 1, 10, 0, 0, 0, entity.JST)},
		{"endAt", ins.EndAt, time.Date(2026, 12, 1, 11, 0, 0, 0, entity.JST)},
		{"deadline", ins.ReservationDeadlineAt, time.Date(2026, 12, 1, 0, 0, 0, 0, entity.JST)},
	}
	for _, c := range cases {
		if !c.got.Equal(c.want) || c.got.Nanosecond() != 0 || c.got.Location() != entity.JST {
			t.Errorf("%s = %v (loc %v), want %v JST・秒未満なし", c.name, c.got, c.got.Location(), c.want)
		}
	}
}

func TestCreate_InsertErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeSlots{insertErr: boom}
	if _, err := newSvc(f).Create(context.Background(), adminActor, validCreate()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

// ---- Get ----------------------------------------------------------------

func TestGet(t *testing.T) {
	ctx := context.Background()
	t.Run("管理者は下書きも見える", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotDraft), remaining: 2}
		got, err := newSvc(f).Get(ctx, adminActor, "p")
		if err != nil {
			t.Fatal(err)
		}
		if got.Slot.PublicID != "p" || got.Remaining != 2 {
			t.Errorf("got = %+v", got)
		}
	})
	t.Run("利用者に下書きは 404(存在を漏らさない)", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotDraft)}
		if _, err := newSvc(f).Get(ctx, userActor, "p"); !errors.Is(err, usecase.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	for _, st := range []entity.SlotStatus{entity.SlotPublished, entity.SlotClosed} {
		t.Run("利用者に "+string(st)+" は 200(N7)", func(t *testing.T) {
			f := &fakeSlots{slot: baseSlot(st), remaining: 1}
			got, err := newSvc(f).Get(ctx, userActor, "p")
			if err != nil {
				t.Fatal(err)
			}
			if got.Remaining != 1 {
				t.Errorf("remaining = %d, want 1", got.Remaining)
			}
		})
	}
	t.Run("締切後の published も利用者に見える(状態でなく時刻の別軸)", func(t *testing.T) {
		s := baseSlot(entity.SlotPublished)
		s.ReservationDeadlineAt = now.Add(-time.Hour) // 締切は過去
		f := &fakeSlots{slot: s}
		if _, err := newSvc(f).Get(ctx, userActor, "p"); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("無い枠は 404 のまま返る", func(t *testing.T) {
		f := &fakeSlots{getErr: usecase.ErrNotFound}
		for _, a := range []usecase.Actor{adminActor, userActor} {
			if _, err := newSvc(f).Get(ctx, a, "p"); !errors.Is(err, usecase.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		}
	})
	t.Run("Get はトランザクションもロックも使わない", func(t *testing.T) {
		f := &fakeSlots{slot: baseSlot(entity.SlotPublished)}
		_, _ = newSvc(f).Get(ctx, userActor, "p")
		wantCalls(t, f, "get")
	})
}

// ---- List(R4・R16) -----------------------------------------------------

func TestList_VisibilityByRole(t *testing.T) {
	ctx := context.Background()
	t.Run("利用者: ReservableAt=now・status は指定されても無視(不正な値でもエラーにしない)", func(t *testing.T) {
		for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotClosed, "bogus"} {
			f := &fakeSlots{}
			if _, err := newSvc(f).List(ctx, userActor, usecase.ListSlotsInput{Status: ptr(st)}); err != nil {
				t.Fatalf("status=%q: err = %v", st, err)
			}
			q := f.listQuery
			if q.ReservableAt == nil || !q.ReservableAt.Equal(now) {
				t.Errorf("ReservableAt = %v, want now(締切ちょうどは締切後=R4 の境界を repository に渡す)", q.ReservableAt)
			}
			if q.Status != nil {
				t.Errorf("Status = %v: 利用者の status 指定は捨てるはず", *q.Status)
			}
		}
	})
	t.Run("管理者: ReservableAt なし(全状態)・status で絞れる", func(t *testing.T) {
		for _, st := range []entity.SlotStatus{entity.SlotDraft, entity.SlotPublished, entity.SlotClosed} {
			f := &fakeSlots{}
			if _, err := newSvc(f).List(ctx, adminActor, usecase.ListSlotsInput{Status: ptr(st)}); err != nil {
				t.Fatal(err)
			}
			q := f.listQuery
			if q.ReservableAt != nil {
				t.Errorf("ReservableAt = %v, 管理者には付けない", *q.ReservableAt)
			}
			if q.Status == nil || *q.Status != st {
				t.Errorf("Status = %v, want %s", q.Status, st)
			}
		}
		f := &fakeSlots{}
		if _, err := newSvc(f).List(ctx, adminActor, usecase.ListSlotsInput{}); err != nil {
			t.Fatal(err)
		}
		if f.listQuery.ReservableAt != nil || f.listQuery.Status != nil {
			t.Errorf("指定なしの管理者は条件なし: %+v", f.listQuery)
		}
	})
	t.Run("管理者の不正な status は 400 で DB に触れない", func(t *testing.T) {
		f := &fakeSlots{}
		_, err := newSvc(f).List(ctx, adminActor, usecase.ListSlotsInput{Status: ptr(entity.SlotStatus("bogus"))})
		wantFields(t, err, "status")
		wantCalls(t, f)
	})
	t.Run("availableOnly・from・to は両者とも素通し(R4: 残席>0 だけを足す)", func(t *testing.T) {
		from := time.Date(2026, 11, 1, 0, 0, 0, 0, entity.JST)
		to := time.Date(2026, 12, 1, 0, 0, 0, 0, entity.JST)
		for name, a := range map[string]usecase.Actor{"利用者": userActor, "管理者": adminActor} {
			f := &fakeSlots{}
			in := usecase.ListSlotsInput{From: &from, To: &to, AvailableOnly: true}
			if _, err := newSvc(f).List(ctx, a, in); err != nil {
				t.Fatal(err)
			}
			q := f.listQuery
			if !q.AvailableOnly || q.From == nil || !q.From.Equal(from) || q.To == nil || !q.To.Equal(to) {
				t.Errorf("%s: query = %+v", name, q)
			}
		}
	})
	t.Run("結果の Items を素通しする", func(t *testing.T) {
		items := []entity.SlotWithRemaining{{Slot: baseSlot(entity.SlotPublished), Remaining: 2}}
		f := &fakeSlots{listItems: items}
		page, err := newSvc(f).List(ctx, userActor, usecase.ListSlotsInput{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(page.Items, items) {
			t.Errorf("Items = %+v", page.Items)
		}
	})
	t.Run("store の障害はそのまま返る", func(t *testing.T) {
		boom := errors.New("boom")
		f := &fakeSlots{listErr: boom}
		if _, err := newSvc(f).List(ctx, userActor, usecase.ListSlotsInput{}); !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
	t.Run("List はトランザクションもロックも使わない", func(t *testing.T) {
		f := &fakeSlots{}
		_, _ = newSvc(f).List(ctx, userActor, usecase.ListSlotsInput{})
		wantCalls(t, f, "list")
	})
}

func TestList_LimitOffsetAndRange(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, entity.JST)
	cases := []struct {
		name       string
		in         usecase.ListSlotsInput
		fields     []string // 空なら通る
		wantLimit  int
		wantOffset int
	}{
		{"指定なしは limit 20・offset 0", usecase.ListSlotsInput{}, nil, 20, 0},
		{"limit 0", usecase.ListSlotsInput{Limit: ptr(0)}, []string{"limit"}, 0, 0},
		{"limit 1", usecase.ListSlotsInput{Limit: ptr(1)}, nil, 1, 0},
		{"limit 100", usecase.ListSlotsInput{Limit: ptr(100)}, nil, 100, 0},
		{"limit 101", usecase.ListSlotsInput{Limit: ptr(101)}, []string{"limit"}, 0, 0},
		{"limit 負", usecase.ListSlotsInput{Limit: ptr(-5)}, []string{"limit"}, 0, 0},
		{"offset -1", usecase.ListSlotsInput{Offset: ptr(-1)}, []string{"offset"}, 0, 0},
		{"offset 0", usecase.ListSlotsInput{Offset: ptr(0)}, nil, 20, 0},
		{"offset 40(limit は既定のまま)", usecase.ListSlotsInput{Offset: ptr(40)}, nil, 20, 40},
		{"limit 5・offset 10", usecase.ListSlotsInput{Limit: ptr(5), Offset: ptr(10)}, nil, 5, 10},
		{"from > to", usecase.ListSlotsInput{From: &t0, To: ptr(t0.Add(-time.Second))}, []string{"from"}, 0, 0},
		{"from = to", usecase.ListSlotsInput{From: &t0, To: &t0}, nil, 20, 0},
		{"from < to", usecase.ListSlotsInput{From: &t0, To: ptr(t0.Add(time.Second))}, nil, 20, 0},
		{"from だけ", usecase.ListSlotsInput{From: &t0}, nil, 20, 0},
		{"to だけ", usecase.ListSlotsInput{To: &t0}, nil, 20, 0},
		{"limit・offset・from>to の全部が不正なら全部返す", usecase.ListSlotsInput{Limit: ptr(0), Offset: ptr(-1), From: &t0, To: ptr(t0.Add(-time.Hour))},
			[]string{"limit", "offset", "from"}, 0, 0},
	}
	for _, actorName := range []string{"利用者", "管理者"} {
		actor := map[string]usecase.Actor{"利用者": userActor, "管理者": adminActor}[actorName]
		for _, tt := range cases {
			t.Run(actorName+"/"+tt.name, func(t *testing.T) {
				f := &fakeSlots{}
				page, err := newSvc(f).List(ctx, actor, tt.in)
				if tt.fields != nil {
					wantFields(t, err, tt.fields...)
					wantCalls(t, f) // 検証に落ちたら DB に触れない
					return
				}
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if page.Limit != tt.wantLimit || page.Offset != tt.wantOffset {
					t.Errorf("page limit/offset = %d/%d, want %d/%d(実効値)", page.Limit, page.Offset, tt.wantLimit, tt.wantOffset)
				}
				if f.listQuery.Limit != tt.wantLimit || f.listQuery.Offset != tt.wantOffset {
					t.Errorf("query limit/offset = %d/%d, want %d/%d", f.listQuery.Limit, f.listQuery.Offset, tt.wantLimit, tt.wantOffset)
				}
			})
		}
	}
}
