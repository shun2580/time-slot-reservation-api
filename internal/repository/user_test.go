package repository

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/repository/repotest"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// UserRepository が usecase.UserStore を満たすこと(コンパイル時の確認)。
var _ usecase.UserStore = (*UserRepository)(nil)

type userRow struct {
	id         int64
	sub        string
	createdRaw string
	updatedRaw string
}

// readUsers は users の全行を id 順に返す。時刻は DB に入った文字列のまま読む
// (ドライバの変換を挟まず、JST で保存されたかを直に見るため)。
func readUsers(t *testing.T, ctx context.Context, db *sql.DB) []userRow {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		"SELECT id, keycloak_sub, CAST(created_at AS CHAR), CAST(updated_at AS CHAR) FROM users ORDER BY id")
	if err != nil {
		t.Fatalf("users を読めない: %v", err)
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var r userRow
		if err := rows.Scan(&r.id, &r.sub, &r.createdRaw, &r.updatedRaw); err != nil {
			t.Fatalf("users の行を読めない: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("users の走査: %v", err)
	}
	return out
}

// 逐次の場面。コンテナは 1 つを Reset で使い回す。
func TestUserRepository_EnsureUser(t *testing.T) {
	db := repotest.StartMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 時刻を差し込む。最初の呼びと 2 回目以降で別の時刻にし、既存行の時刻が動かぬことを見分けられるようにする。
	// 1 回目は UTC で渡す(2026-03-01 00:30:00Z = JST 09:30:00)。保存が JST に直るかを見る。
	first := time.Date(2026, 3, 1, 0, 30, 0, 0, time.UTC)
	later := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	clock := first
	repo := NewUserRepository(db, func() time.Time { return clock })

	t.Run("初回で1行作られidが返り、時刻はJSTで保存される", func(t *testing.T) {
		repotest.Reset(t, db)
		clock = first

		id, err := repo.EnsureUser(ctx, "sub-a")
		if err != nil {
			t.Fatalf("EnsureUser: %v", err)
		}
		if id <= 0 {
			t.Errorf("id = %d, want 正の値", id)
		}
		got := readUsers(t, ctx, db)
		if len(got) != 1 {
			t.Fatalf("行数 = %d, want 1", len(got))
		}
		if got[0].id != id || got[0].sub != "sub-a" {
			t.Errorf("行 = %+v, want id=%d sub=sub-a", got[0], id)
		}
		const wantJST = "2026-03-01 09:30:00"
		if got[0].createdRaw != wantJST || got[0].updatedRaw != wantJST {
			t.Errorf("保存された時刻 created=%q updated=%q, want 両方 %q(JST)", got[0].createdRaw, got[0].updatedRaw, wantJST)
		}
	})

	t.Run("同じsubの再アクセスは同じid・行は増えず時刻も変わらない", func(t *testing.T) {
		repotest.Reset(t, db)
		clock = first
		id1, err := repo.EnsureUser(ctx, "sub-a")
		if err != nil {
			t.Fatalf("1回目: %v", err)
		}
		before := readUsers(t, ctx, db)

		clock = later // 2 回目の時刻は別。既存行が書き換わればここで差が出る。
		id2, err := repo.EnsureUser(ctx, "sub-a")
		if err != nil {
			t.Fatalf("2回目: %v", err)
		}
		if id2 != id1 {
			t.Errorf("2回目の id = %d, want 1回目と同じ %d", id2, id1)
		}
		after := readUsers(t, ctx, db)
		if len(after) != 1 {
			t.Fatalf("行数 = %d, want 1", len(after))
		}
		if after[0] != before[0] {
			t.Errorf("既存行が変わった: before=%+v after=%+v", before[0], after[0])
		}
	})

	t.Run("別のsubは別のid", func(t *testing.T) {
		repotest.Reset(t, db)
		clock = first
		idA, err := repo.EnsureUser(ctx, "sub-a")
		if err != nil {
			t.Fatalf("sub-a: %v", err)
		}
		idB, err := repo.EnsureUser(ctx, "sub-b")
		if err != nil {
			t.Fatalf("sub-b: %v", err)
		}
		if idA == idB {
			t.Errorf("別の sub なのに同じ id %d", idA)
		}
		// 既存の sub-a を再び呼んでも、sub-b の id に化けない(LAST_INSERT_ID の取り違え防止)。
		idA2, err := repo.EnsureUser(ctx, "sub-a")
		if err != nil {
			t.Fatalf("sub-a 再: %v", err)
		}
		if idA2 != idA {
			t.Errorf("sub-a の再アクセスの id = %d, want %d", idA2, idA)
		}
		if got := readUsers(t, ctx, db); len(got) != 2 {
			t.Errorf("行数 = %d, want 2", len(got))
		}
	})

	t.Run("nowを差さなくても現在時刻がJSTで入る", func(t *testing.T) {
		repotest.Reset(t, db)
		def := NewUserRepository(db, nil)
		lo := time.Now().In(entity.JST).Add(-2 * time.Second).Truncate(time.Second)
		if _, err := def.EnsureUser(ctx, "sub-now"); err != nil {
			t.Fatalf("EnsureUser: %v", err)
		}
		hi := time.Now().In(entity.JST).Add(2 * time.Second)

		var created time.Time
		if err := db.QueryRowContext(ctx, "SELECT created_at FROM users WHERE keycloak_sub = 'sub-now'").Scan(&created); err != nil {
			t.Fatalf("created_at を読めない: %v", err)
		}
		// 接続は loc=Asia/Tokyo。JST の壁時計として読み戻した値が、現在の JST の範囲に収まること。
		if created.Before(lo) || created.After(hi) {
			t.Errorf("created_at = %v, want %v〜%v の間", created, lo, hi)
		}
	})
}

// 競合の場面。同じ sub で多数が同時に初回アクセスしても、行は 1 つ・全員が同じ id。
func TestUserRepository_EnsureUser_Concurrent(t *testing.T) {
	db := repotest.StartMySQL(t)
	// 同時接続を十分に確保する(既定は無制限だが、明示して 32 本が本当に並ぶようにする)。
	const workers = 32
	db.SetMaxOpenConns(workers)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	repo := NewUserRepository(db, nil)

	ids := make([]int64, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 全員そろってから一斉に走らせる
			ids[i], errs[i] = repo.EnsureUser(ctx, "sub-race")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d が失敗: %v", i, err)
		}
	}
	rows := readUsers(t, ctx, db)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d, want 1", len(rows))
	}
	for i, id := range ids {
		if id != rows[0].id {
			t.Errorf("goroutine %d の id = %d, want 行の id %d", i, id, rows[0].id)
		}
	}
}
