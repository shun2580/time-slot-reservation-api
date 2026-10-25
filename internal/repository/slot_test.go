package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/repository"
	"github.com/shun2580/time-slot-reservation-api/internal/repository/repotest"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// MySQL のエラー番号(FK 違反: 親行を消せない)。
const mysqlErrRowIsReferenced = 1451

// at は JST の時刻を作る。
func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, entity.JST)
	if err != nil {
		panic(err)
	}
	return t
}

// seedSlot は枠を repository の Insert で1行入れる(Insert 自体の試験は別に行う)。
func seedSlot(t *testing.T, r *repository.SlotRepository, title string, capacity int, start, deadline string, st entity.SlotStatus) entity.Slot {
	t.Helper()
	s, err := r.Insert(context.Background(), entity.Slot{
		PublicID: entity.NewPublicID(), Title: title, Capacity: capacity,
		StartAt: at(start), EndAt: at(start).Add(time.Hour), ReservationDeadlineAt: at(deadline),
		Status: st, CreatedAt: at("2026-01-01 00:00:00"), UpdatedAt: at("2026-01-01 00:00:00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seedUser は users に 1 行直接入れる(予約の前提づくり。users の repository は別にある)。
func seedUser(t *testing.T, db *sql.DB, sub string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO users (keycloak_sub, created_at, updated_at) VALUES (?, NOW(), NOW())`, sub)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedReservation は reservations に 1 行直接入れる。
// 予約の repository は殿が書くので、枠の repository の試験では SQL で前提を作る。
func seedReservation(t *testing.T, db *sql.DB, slotID, userID int64, status entity.ReservationStatus) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, NOW(), NOW())`,
		entity.NewPublicID(), slotID, userID, string(status)); err != nil {
		t.Fatal(err)
	}
}

func titles(items []entity.SlotWithRemaining) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.Title)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSlotRepository は実 MySQL(testcontainers)で SlotRepository と TxManager を確かめる。
// コンテナの起動は約 11 秒かかるので 1 つだけ立て、場面ごとに repotest.Reset で表を空に戻して使い回す。
// 場面は順に走らせる(t.Parallel を使わない。同じ DB を共有するため)。
func TestSlotRepository(t *testing.T) {
	db := repotest.StartMySQL(t)
	ctx := context.Background()
	repo := repository.NewSlotRepository(db)
	txm := repository.NewTxManager(db)

	list := func(t *testing.T, q usecase.SlotListQuery) []entity.SlotWithRemaining {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = 20
		}
		items, err := repo.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	// lockedOf は、tx の外から枠の行を読むだけの試験用 LockedSlot。
	// 本番では LockByPublicID だけがこれを作る。ここでは「予約の有無」の SQL を確かめたいので特別に組み立てる。
	lockedOf := func(s entity.Slot) usecase.LockedSlot { return usecase.LockedSlot{Slot: s} }

	t.Run("Insert は ID を埋め、値を往復できる", func(t *testing.T) {
		repotest.Reset(t, db)
		in := entity.Slot{
			PublicID: entity.NewPublicID(), Title: "朝のヨガ", Capacity: 12,
			StartAt: at("2026-11-01 09:00:00"), EndAt: at("2026-11-01 10:30:00"), ReservationDeadlineAt: at("2026-10-31 18:00:00"),
			Status: entity.SlotDraft, CreatedAt: at("2026-10-25 12:00:00"), UpdatedAt: at("2026-10-25 12:00:00"),
		}
		first, err := repo.Insert(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		in.PublicID = entity.NewPublicID()
		second, err := repo.Insert(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID <= 0 || second.ID != first.ID+1 {
			t.Errorf("ID: first=%d second=%d (連番で埋まるはず)", first.ID, second.ID)
		}

		got, err := repo.GetWithRemaining(ctx, first.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != first.ID || got.Title != "朝のヨガ" || got.Capacity != 12 || got.Status != entity.SlotDraft {
			t.Errorf("往復した値が違う: %+v", got)
		}
		if got.Remaining != 12 {
			t.Errorf("予約が無いので Remaining = Capacity のはず: %d", got.Remaining)
		}
		for name, c := range map[string]struct{ got, want time.Time }{
			"StartAt":               {got.StartAt, in.StartAt},
			"EndAt":                 {got.EndAt, in.EndAt},
			"ReservationDeadlineAt": {got.ReservationDeadlineAt, in.ReservationDeadlineAt},
			"CreatedAt":             {got.CreatedAt, in.CreatedAt},
			"UpdatedAt":             {got.UpdatedAt, in.UpdatedAt},
		} {
			if !c.got.Equal(c.want) {
				t.Errorf("%s: got %s want %s", name, c.got.In(entity.JST).Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		}
	})

	t.Run("一覧の並びは start_at 昇順、同刻は id 昇順", func(t *testing.T) {
		repotest.Reset(t, db)
		// 挿入順をわざと start_at と食い違わせる: 後ろの日→同刻(2枠)→前の日。
		late := seedSlot(t, repo, "late", 5, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotPublished)
		tieA := seedSlot(t, repo, "tieA", 5, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		tieB := seedSlot(t, repo, "tieB", 5, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		early := seedSlot(t, repo, "early", 5, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		_ = late

		got := titles(list(t, usecase.SlotListQuery{}))
		want := []string{early.Title, tieA.Title, tieB.Title, "late"}
		if !equalStrings(got, want) {
			t.Errorf("並び: got %v want %v", got, want)
		}
	})

	t.Run("0件でも nil でなく空スライス", func(t *testing.T) {
		repotest.Reset(t, db)
		items, err := repo.List(ctx, usecase.SlotListQuery{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if items == nil || len(items) != 0 {
			t.Errorf("want 空スライス(非 nil), got nil=%v len=%d", items == nil, len(items))
		}
	})

	t.Run("残席は満杯・空き・取消のみで正しい", func(t *testing.T) {
		repotest.Reset(t, db)
		u1, u2, u3 := seedUser(t, db, "u1"), seedUser(t, db, "u2"), seedUser(t, db, "u3")
		full := seedSlot(t, repo, "full", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		part := seedSlot(t, repo, "part", 3, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		none := seedSlot(t, repo, "none", 4, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotPublished)
		cancelledOnly := seedSlot(t, repo, "cancelledOnly", 2, "2026-11-04 09:00:00", "2026-11-04 08:00:00", entity.SlotClosed)
		seedReservation(t, db, full.ID, u1, entity.ReservationActive)
		seedReservation(t, db, full.ID, u2, entity.ReservationActive)
		seedReservation(t, db, part.ID, u1, entity.ReservationActive)
		seedReservation(t, db, part.ID, u2, entity.ReservationCancelledByUser) // 取消は数えない
		seedReservation(t, db, part.ID, u3, entity.ReservationCancelledByAdmin)
		seedReservation(t, db, cancelledOnly.ID, u1, entity.ReservationCancelledByUser)
		_ = none

		want := map[string]int{"full": 0, "part": 2, "none": 4, "cancelledOnly": 2}
		items := list(t, usecase.SlotListQuery{})
		if len(items) != len(want) {
			t.Fatalf("件数: %v", titles(items))
		}
		for _, it := range items {
			if it.Remaining != want[it.Title] {
				t.Errorf("一覧 %s: Remaining=%d want %d", it.Title, it.Remaining, want[it.Title])
			}
			// 詳細(GetWithRemaining)も同じ値になる
			g, err := repo.GetWithRemaining(ctx, it.PublicID)
			if err != nil {
				t.Fatal(err)
			}
			if g.Remaining != want[it.Title] {
				t.Errorf("詳細 %s: Remaining=%d want %d", it.Title, g.Remaining, want[it.Title])
			}
		}
	})

	t.Run("GetWithRemaining は無ければ ErrNotFound", func(t *testing.T) {
		repotest.Reset(t, db)
		if _, err := repo.GetWithRemaining(ctx, entity.NewPublicID()); !errors.Is(err, usecase.ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("利用者の見える範囲は published かつ now<締切(ちょうどは見えない=R4)", func(t *testing.T) {
		repotest.Reset(t, db)
		now := at("2026-10-25 21:00:00")
		// 締切: now の 1 秒前 / ちょうど now / 1 秒後
		seedSlot(t, repo, "published-deadline-1s-before", 3, "2026-11-01 09:00:00", "2026-10-25 20:59:59", entity.SlotPublished)
		seedSlot(t, repo, "published-deadline-exactly-now", 3, "2026-11-02 09:00:00", "2026-10-25 21:00:00", entity.SlotPublished)
		seedSlot(t, repo, "published-deadline-1s-after", 3, "2026-11-03 09:00:00", "2026-10-25 21:00:01", entity.SlotPublished)
		seedSlot(t, repo, "draft-future", 3, "2026-11-04 09:00:00", "2026-11-04 08:00:00", entity.SlotDraft)
		seedSlot(t, repo, "closed-future", 3, "2026-11-05 09:00:00", "2026-11-05 08:00:00", entity.SlotClosed)

		got := titles(list(t, usecase.SlotListQuery{ReservableAt: &now}))
		want := []string{"published-deadline-1s-after"}
		if !equalStrings(got, want) {
			t.Errorf("利用者の見える枠: got %v want %v", got, want)
		}
		// 管理者(ReservableAt なし)は全状態・全時期が見える
		if all := list(t, usecase.SlotListQuery{}); len(all) != 5 {
			t.Errorf("管理者は 5 件のはず: %v", titles(all))
		}
	})

	t.Run("availableOnly は残席>0 だけ。ReservableAt と掛け合わせても正しい", func(t *testing.T) {
		repotest.Reset(t, db)
		now := at("2026-10-25 21:00:00")
		u1, u2, u3 := seedUser(t, db, "u1"), seedUser(t, db, "u2"), seedUser(t, db, "u3")
		open := seedSlot(t, repo, "open", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		full := seedSlot(t, repo, "full", 1, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		cancelled := seedSlot(t, repo, "cancelled-only", 1, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotPublished)
		draftOpen := seedSlot(t, repo, "draft-open", 2, "2026-11-04 09:00:00", "2026-11-04 08:00:00", entity.SlotDraft)
		pastDeadline := seedSlot(t, repo, "past-deadline-open", 2, "2026-11-05 09:00:00", "2026-10-01 08:00:00", entity.SlotPublished)
		seedReservation(t, db, open.ID, u1, entity.ReservationActive)
		seedReservation(t, db, full.ID, u1, entity.ReservationActive)
		seedReservation(t, db, cancelled.ID, u2, entity.ReservationCancelledByUser)
		seedReservation(t, db, cancelled.ID, u3, entity.ReservationCancelledByAdmin)
		_, _ = draftOpen, pastDeadline

		// 利用者の見える範囲(open / full / cancelled-only)のうち、空きがあるのは open と cancelled-only
		user := titles(list(t, usecase.SlotListQuery{ReservableAt: &now, AvailableOnly: true}))
		if want := []string{"open", "cancelled-only"}; !equalStrings(user, want) {
			t.Errorf("利用者 availableOnly: got %v want %v", user, want)
		}
		// 管理者(ReservableAt なし)は draft・締切後も含めて「満杯でない」全部
		admin := titles(list(t, usecase.SlotListQuery{AvailableOnly: true}))
		if want := []string{"open", "cancelled-only", "draft-open", "past-deadline-open"}; !equalStrings(admin, want) {
			t.Errorf("管理者 availableOnly: got %v want %v", admin, want)
		}
		// availableOnly を付けなければ満杯も出る
		if all := list(t, usecase.SlotListQuery{ReservableAt: &now}); len(all) != 3 {
			t.Errorf("availableOnly 無しの利用者は 3 件: %v", titles(all))
		}
	})

	t.Run("status・from・to・limit・offset の絞り込み", func(t *testing.T) {
		repotest.Reset(t, db)
		d := seedSlot(t, repo, "d", 3, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotDraft)
		p1 := seedSlot(t, repo, "p1", 3, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		p2 := seedSlot(t, repo, "p2", 3, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotPublished)
		c := seedSlot(t, repo, "c", 3, "2026-11-04 09:00:00", "2026-11-04 08:00:00", entity.SlotClosed)
		_, _, _, _ = d, p1, p2, c

		// status
		for st, want := range map[entity.SlotStatus][]string{
			entity.SlotDraft:     {"d"},
			entity.SlotPublished: {"p1", "p2"},
			entity.SlotClosed:    {"c"},
		} {
			st := st
			if got := titles(list(t, usecase.SlotListQuery{Status: &st})); !equalStrings(got, want) {
				t.Errorf("status=%s: got %v want %v", st, got, want)
			}
		}

		// from は以上、to は未満(境界ちょうどの p1=11-02 09:00 を from に、p2=11-03 09:00 を to に置く)
		from, to := at("2026-11-02 09:00:00"), at("2026-11-03 09:00:00")
		if got := titles(list(t, usecase.SlotListQuery{From: &from})); !equalStrings(got, []string{"p1", "p2", "c"}) {
			t.Errorf("from は以上: %v", got)
		}
		if got := titles(list(t, usecase.SlotListQuery{To: &to})); !equalStrings(got, []string{"d", "p1"}) {
			t.Errorf("to は未満(p2 は含まれない): %v", got)
		}
		if got := titles(list(t, usecase.SlotListQuery{From: &from, To: &to})); !equalStrings(got, []string{"p1"}) {
			t.Errorf("from 以上 to 未満: %v", got)
		}
		// from == to は空(半開区間)
		if got := list(t, usecase.SlotListQuery{From: &from, To: &from}); len(got) != 0 {
			t.Errorf("from==to は空のはず: %v", titles(got))
		}

		// status と from の AND
		pub := entity.SlotPublished
		if got := titles(list(t, usecase.SlotListQuery{Status: &pub, From: &to})); !equalStrings(got, []string{"p2"}) {
			t.Errorf("status AND from: %v", got)
		}

		// limit / offset
		if got := titles(list(t, usecase.SlotListQuery{Limit: 2})); !equalStrings(got, []string{"d", "p1"}) {
			t.Errorf("limit=2: %v", got)
		}
		if got := titles(list(t, usecase.SlotListQuery{Limit: 2, Offset: 1})); !equalStrings(got, []string{"p1", "p2"}) {
			t.Errorf("limit=2 offset=1: %v", got)
		}
		if got := titles(list(t, usecase.SlotListQuery{Limit: 2, Offset: 3})); !equalStrings(got, []string{"c"}) {
			t.Errorf("limit=2 offset=3 (残り1件): %v", got)
		}
		if got := list(t, usecase.SlotListQuery{Limit: 2, Offset: 4}); len(got) != 0 {
			t.Errorf("offset が件数以上なら空: %v", titles(got))
		}
	})

	t.Run("HasAnyReservation は取消のみでも true、HasActiveReservation は false", func(t *testing.T) {
		repotest.Reset(t, db)
		u1, u2 := seedUser(t, db, "u1"), seedUser(t, db, "u2")
		empty := seedSlot(t, repo, "empty", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		cancelledOnly := seedSlot(t, repo, "cancelled-only", 2, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		activeOnly := seedSlot(t, repo, "active-only", 2, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotPublished)
		mixed := seedSlot(t, repo, "mixed", 2, "2026-11-04 09:00:00", "2026-11-04 08:00:00", entity.SlotPublished)
		seedReservation(t, db, cancelledOnly.ID, u1, entity.ReservationCancelledByUser)
		seedReservation(t, db, cancelledOnly.ID, u2, entity.ReservationCancelledByAdmin)
		seedReservation(t, db, activeOnly.ID, u1, entity.ReservationActive)
		seedReservation(t, db, mixed.ID, u1, entity.ReservationCancelledByUser)
		seedReservation(t, db, mixed.ID, u2, entity.ReservationActive)

		for _, c := range []struct {
			slot        entity.Slot
			any, active bool
		}{
			{empty, false, false},
			{cancelledOnly, true, false}, // R1: 取消済みも「予約が入っている」
			{activeOnly, true, true},
			{mixed, true, true},
		} {
			any, err := repo.HasAnyReservation(ctx, lockedOf(c.slot))
			if err != nil {
				t.Fatal(err)
			}
			active, err := repo.HasActiveReservation(ctx, lockedOf(c.slot))
			if err != nil {
				t.Fatal(err)
			}
			if any != c.any || active != c.active {
				t.Errorf("%s: any=%v active=%v want any=%v active=%v", c.slot.Title, any, active, c.any, c.active)
			}
		}
	})

	t.Run("UpdateFields・UpdateStatus は対象の枠だけに反映される", func(t *testing.T) {
		repotest.Reset(t, db)
		target := seedSlot(t, repo, "target", 5, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotDraft)
		other := seedSlot(t, repo, "other", 5, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotDraft)

		err := txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			l, err := tx.Slots().LockByPublicID(ctx, target.PublicID)
			if err != nil {
				return err
			}
			to := l.Slot
			to.Title, to.Capacity = "changed", 8
			to.StartAt, to.EndAt = at("2026-12-01 10:00:00"), at("2026-12-01 11:30:00")
			to.ReservationDeadlineAt = at("2026-12-01 09:00:00")
			to.UpdatedAt = at("2026-10-25 22:00:00")
			if err := tx.Slots().UpdateFields(ctx, l, to); err != nil {
				return err
			}
			return tx.Slots().UpdateStatus(ctx, l, entity.SlotPublished, at("2026-10-25 22:05:00"))
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := repo.GetWithRemaining(ctx, target.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "changed" || got.Capacity != 8 || got.Status != entity.SlotPublished {
			t.Errorf("反映されていない: %+v", got)
		}
		if !got.StartAt.Equal(at("2026-12-01 10:00:00")) || !got.EndAt.Equal(at("2026-12-01 11:30:00")) ||
			!got.ReservationDeadlineAt.Equal(at("2026-12-01 09:00:00")) {
			t.Errorf("日時: start=%s end=%s deadline=%s", got.StartAt, got.EndAt, got.ReservationDeadlineAt)
		}
		// UpdateStatus の updated_at が最後に書かれる(UpdateFields の値で上書きされたまま、ではない)
		if !got.UpdatedAt.Equal(at("2026-10-25 22:05:00")) {
			t.Errorf("updated_at: %s", got.UpdatedAt)
		}
		// 作成時刻・公開 ID は変わらない
		if !got.CreatedAt.Equal(target.CreatedAt) || got.PublicID != target.PublicID {
			t.Errorf("created_at / public_id が変わった: %+v", got)
		}
		// 別の枠は触られていない
		o, err := repo.GetWithRemaining(ctx, other.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if o.Title != "other" || o.Capacity != 5 || o.Status != entity.SlotDraft || !o.UpdatedAt.Equal(other.UpdatedAt) {
			t.Errorf("他の枠が変わった: %+v", o)
		}
	})

	t.Run("LockByPublicID は無ければ ErrNotFound、あれば枠の全列を返す", func(t *testing.T) {
		repotest.Reset(t, db)
		s := seedSlot(t, repo, "locked", 5, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		err := txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			l, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
			if err != nil {
				return err
			}
			if l.Slot.ID != s.ID || l.Slot.Title != "locked" || l.Slot.Status != entity.SlotPublished {
				t.Errorf("ロックした枠: %+v", l.Slot)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			_, err := tx.Slots().LockByPublicID(ctx, entity.NewPublicID())
			return err
		})
		if !errors.Is(err, usecase.ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("Delete は予約の無い枠を消し、予約のある枠は FK が拒む", func(t *testing.T) {
		repotest.Reset(t, db)
		u1 := seedUser(t, db, "u1")
		bare := seedSlot(t, repo, "bare", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotDraft)
		withActive := seedSlot(t, repo, "with-active", 2, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)
		withCancelled := seedSlot(t, repo, "with-cancelled", 2, "2026-11-03 09:00:00", "2026-11-03 08:00:00", entity.SlotClosed)
		seedReservation(t, db, withActive.ID, u1, entity.ReservationActive)
		seedReservation(t, db, withCancelled.ID, u1, entity.ReservationCancelledByUser)

		del := func(publicID string) error {
			return txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
				l, err := tx.Slots().LockByPublicID(ctx, publicID)
				if err != nil {
					return err
				}
				return tx.Slots().Delete(ctx, l)
			})
		}

		if err := del(bare.PublicID); err != nil {
			t.Fatalf("予約の無い枠は消えるはず: %v", err)
		}
		if _, err := repo.GetWithRemaining(ctx, bare.PublicID); !errors.Is(err, usecase.ErrNotFound) {
			t.Errorf("消した枠が残っている: %v", err)
		}

		for _, s := range []entity.Slot{withActive, withCancelled} {
			err := del(s.PublicID)
			var me *mysql.MySQLError
			if !errors.As(err, &me) || me.Number != mysqlErrRowIsReferenced {
				t.Errorf("%s: want MySQL error 1451 (FK RESTRICT), got %v", s.Title, err)
			}
			// 拒まれたら枠は残る
			if _, err := repo.GetWithRemaining(ctx, s.PublicID); err != nil {
				t.Errorf("%s: 拒まれたのに枠が消えた: %v", s.Title, err)
			}
		}
	})

	t.Run("FOR UPDATE: tx1 が握る間 tx2 は待たされ、commit 後に通る", func(t *testing.T) {
		repotest.Reset(t, db)
		s := seedSlot(t, repo, "contended", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)

		locked := make(chan struct{})
		release := make(chan struct{})
		done1 := make(chan error, 1)
		go func() {
			done1 <- txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
				if _, err := tx.Slots().LockByPublicID(ctx, s.PublicID); err != nil {
					return err
				}
				close(locked)
				<-release
				return nil
			})
		}()
		select {
		case <-locked:
		case <-time.After(10 * time.Second):
			t.Fatal("tx1 がロックを取れない")
		}

		type result struct {
			at  time.Time
			err error
		}
		done2 := make(chan result, 1)
		go func() {
			var acquired time.Time
			err := txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
				_, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
				acquired = time.Now()
				return err
			})
			done2 <- result{acquired, err}
		}()

		// tx1 が握っている間、tx2 は通らない。
		select {
		case r := <-done2:
			t.Fatalf("tx2 が FOR UPDATE に待たされず通った (err=%v)", r.err)
		case <-time.After(700 * time.Millisecond):
		}

		// tx1 を commit すると、tx2 が通る。
		releasedAt := time.Now()
		close(release)
		if err := <-done1; err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done2:
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.at.Before(releasedAt) {
				t.Errorf("tx2 が tx1 の release より前にロックを取った")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("tx1 の commit 後も tx2 が通らない")
		}
	})

	t.Run("WithinTx: fn が error → 書込みが残らず、error は包み直されない", func(t *testing.T) {
		repotest.Reset(t, db)
		s := seedSlot(t, repo, "rollback", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotDraft)

		bizErr := usecase.Business(usecase.CodeInvalidStateTransition, "x")
		err := txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			l, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
			if err != nil {
				return err
			}
			if err := tx.Slots().UpdateStatus(ctx, l, entity.SlotPublished, at("2026-10-25 22:00:00")); err != nil {
				return err
			}
			return bizErr
		})
		if err != bizErr {
			t.Errorf("fn の error をそのまま返すはず: got %v", err)
		}
		if !usecase.IsBusiness(err, usecase.CodeInvalidStateTransition) {
			t.Errorf("業務エラーを取り出せない: %v", err)
		}
		got, err := repo.GetWithRemaining(ctx, s.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != entity.SlotDraft || !got.UpdatedAt.Equal(s.UpdatedAt) {
			t.Errorf("rollback されていない: status=%s updated_at=%s", got.Status, got.UpdatedAt)
		}

		// 比較として、error を返さなければ commit される
		err = txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			l, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
			if err != nil {
				return err
			}
			return tx.Slots().UpdateStatus(ctx, l, entity.SlotPublished, at("2026-10-25 22:00:00"))
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := repo.GetWithRemaining(ctx, s.PublicID); got.Status != entity.SlotPublished {
			t.Errorf("commit されていない: %s", got.Status)
		}
	})

	t.Run("WithinTx: panic でも rollback し、同じ panic を再び投げ、接続を返す", func(t *testing.T) {
		repotest.Reset(t, db)
		s := seedSlot(t, repo, "panic", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotDraft)

		boom := errors.New("boom")
		func() {
			defer func() {
				if r := recover(); r != boom {
					t.Errorf("同じ panic 値を再び投げるはず: recovered %v", r)
				}
			}()
			_ = txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
				l, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
				if err != nil {
					return err
				}
				if err := tx.Slots().UpdateStatus(ctx, l, entity.SlotPublished, at("2026-10-25 22:00:00")); err != nil {
					return err
				}
				panic(boom)
			})
			t.Error("panic が呼び手へ伝わらなかった")
		}()

		got, err := repo.GetWithRemaining(ctx, s.PublicID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != entity.SlotDraft {
			t.Errorf("panic 時に rollback されていない: %s", got.Status)
		}
		// rollback されたので行ロックも外れている: すぐに次の tx がロックを取れる
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err = txm.WithinTx(ctx2, func(ctx context.Context, tx usecase.Tx) error {
			_, err := tx.Slots().LockByPublicID(ctx, s.PublicID)
			return err
		})
		if err != nil {
			t.Errorf("panic の後に行ロックが残っている: %v", err)
		}
		if n := db.Stats().InUse; n != 0 {
			t.Errorf("接続が返っていない: InUse=%d", n)
		}
	})

	t.Run("WithinTx は READ COMMITTED: tx の途中で他が commit した予約が見える", func(t *testing.T) {
		repotest.Reset(t, db)
		u1 := seedUser(t, db, "u1")
		watched := seedSlot(t, repo, "watched", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		held := seedSlot(t, repo, "held", 2, "2026-11-02 09:00:00", "2026-11-02 08:00:00", entity.SlotPublished)

		// tx は held の行を握る。watched の予約は、tx の外で後から commit される。
		// (watched をロックすると外の INSERT が FK の共有ロックで待たされるので、別の枠 held を握る。)
		// HasActiveReservation は通常の SELECT(一貫性読み取り)。
		//   REPEATABLE READ: 最初の読みでスナップショットが決まり、後から commit された予約は見えない。
		//   READ COMMITTED : 各文が最新の確定行を読むので、見える。
		err := txm.WithinTx(ctx, func(ctx context.Context, tx usecase.Tx) error {
			if _, err := tx.Slots().LockByPublicID(ctx, held.PublicID); err != nil {
				return err
			}
			before, err := tx.Slots().HasActiveReservation(ctx, lockedOf(watched))
			if err != nil {
				return err
			}
			if before {
				t.Error("前提: 最初は有効な予約が無いはず")
			}
			seedReservation(t, db, watched.ID, u1, entity.ReservationActive) // 別の接続で commit 済み
			after, err := tx.Slots().HasActiveReservation(ctx, lockedOf(watched))
			if err != nil {
				return err
			}
			if !after {
				t.Error("READ COMMITTED なら、他が commit した予約が同じ tx の次の文で見えるはず")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reservations への書込みを枠の repository は持たない(読取のみ)", func(t *testing.T) {
		repotest.Reset(t, db)
		u1 := seedUser(t, db, "u1")
		s := seedSlot(t, repo, "readonly", 2, "2026-11-01 09:00:00", "2026-11-01 08:00:00", entity.SlotPublished)
		seedReservation(t, db, s.ID, u1, entity.ReservationActive)

		count := func() (n int) {
			if err := db.QueryRow(`SELECT COUNT(*) FROM reservations`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return
		}
		before := count()
		_ = list(t, usecase.SlotListQuery{})
		if _, err := repo.GetWithRemaining(ctx, s.PublicID); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.HasAnyReservation(ctx, lockedOf(s)); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.HasActiveReservation(ctx, lockedOf(s)); err != nil {
			t.Fatal(err)
		}
		if after := count(); after != before {
			t.Errorf("reservations の行数が変わった: %d → %d", before, after)
		}
		var status string
		if err := db.QueryRow(`SELECT status FROM reservations WHERE slot_id = ?`, s.ID).Scan(&status); err != nil || status != string(entity.ReservationActive) {
			t.Errorf("予約の状態が変わった: %q %v", status, err)
		}
	})
}
