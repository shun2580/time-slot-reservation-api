package repotest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// mysqlCheckViolation は CHECK 制約違反(ER_CHECK_CONSTRAINT_VIOLATED)の番号。
const mysqlCheckViolation = 3819

// Docker を要しない。mysqlImage が docker-compose.yml の mysql サービスの画像と同じことを確かめる。
func TestMySQLImageMatchesCompose(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(RepoRoot(), "docker-compose.yml"))
	if err != nil {
		t.Fatalf("docker-compose.yml を読めない: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s+image:\s+(mysql:\S+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("docker-compose.yml に mysql の image 行が見つからない")
	}
	if got := string(m[1]); got != mysqlImage {
		t.Errorf("mysqlImage = %q, docker-compose.yml = %q: 版をそろえよ", mysqlImage, got)
	}
}

// Docker を要する。Docker が無ければ StartMySQL が t.Fatal で失敗する(Skip しない)。
func TestStartMySQL_Smoke(t *testing.T) {
	db := StartMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 4表が在り、notification_outbox は無い(第1段階では作らない)。
	for _, table := range []string{"users", "slots", "reservations", "reservation_histories"} {
		if !tableExists(t, ctx, db, table) {
			t.Errorf("表 %s が無い", table)
		}
	}
	if tableExists(t, ctx, db, "notification_outbox") {
		t.Error("notification_outbox が在る(第1段階では作らぬ)")
	}

	// schema_migrations は最新版で dirty=false。
	var version uint64
	var dirty bool
	if err := db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		t.Fatalf("schema_migrations を読めない: %v", err)
	}
	if want := latestMigrationVersion(t); version != want {
		t.Errorf("schema_migrations.version = %d, 最新 = %d", version, want)
	}
	if dirty {
		t.Error("schema_migrations.dirty = true")
	}

	// 接続のタイムゾーンは JST。
	var tz string
	if err := db.QueryRowContext(ctx, "SELECT @@session.time_zone").Scan(&tz); err != nil {
		t.Fatalf("time_zone を読めない: %v", err)
	}
	if tz != "+09:00" {
		t.Errorf("session time_zone = %q, want +09:00", tz)
	}

	// slots の CHECK(start_at < end_at)。対照として正しい行は入ること、
	// 違反の行は CHECK 違反(3819)で落ちることを見る(別の理由の失敗を見逃さぬため)。
	const insert = `INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES (?, 't', 1, ?, ?, ?, 'draft', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`
	const (
		early = "2026-02-01 09:00:00"
		late  = "2026-02-01 10:00:00"
	)
	if _, err := db.ExecContext(ctx, insert, "00000000-0000-4000-8000-000000000001", early, late, early); err != nil {
		t.Fatalf("正しい slots 行が入らない: %v", err)
	}
	_, err := db.ExecContext(ctx, insert, "00000000-0000-4000-8000-000000000002", late, early, early)
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) || myErr.Number != mysqlCheckViolation {
		t.Errorf("start_at >= end_at の INSERT が CHECK 違反で落ちなかった: err = %v", err)
	}

	// Reset で表が空に戻る。
	Reset(t, db)
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM slots").Scan(&n); err != nil {
		t.Fatalf("slots を数えられない: %v", err)
	}
	if n != 0 {
		t.Errorf("Reset 後の slots 行数 = %d, want 0", n)
	}
}

func tableExists(t *testing.T, ctx context.Context, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&n)
	if err != nil {
		t.Fatalf("information_schema.tables を読めない: %v", err)
	}
	return n > 0
}

var upFile = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)

// latestMigrationVersion は migrations/ の *.up.sql の最大の番号を返す。
func latestMigrationVersion(t *testing.T) uint64 {
	t.Helper()
	entries, err := os.ReadDir(MigrationsDir())
	if err != nil {
		t.Fatalf("migrations/ を読めない: %v", err)
	}
	var latest uint64
	for _, e := range entries {
		m := upFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Fatalf("番号を読めない %s: %v", e.Name(), err)
		}
		latest = max(latest, v)
	}
	if latest == 0 {
		t.Fatal("migrations/ に *.up.sql が無い")
	}
	return latest
}

// 実DBで migrations を up → down → up と往復し、どちらの up も通ることを見る。
func TestMigrations_RoundTrip(t *testing.T) {
	db, dsn := startMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tables := []string{"users", "slots", "reservations", "reservation_histories"}

	m := newMigrate(t, dsn)
	defer closeMigrate(t, m)

	if err := m.Down(); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	for _, table := range tables {
		if tableExists(t, ctx, db, table) {
			t.Errorf("down の後も表 %s が残っている", table)
		}
	}
	if err := m.Up(); err != nil {
		t.Fatalf("2回目の migrate up: %v", err)
	}
	for _, table := range tables {
		if !tableExists(t, ctx, db, table) {
			t.Errorf("往復後に表 %s が無い", table)
		}
	}
	var dirty bool
	if err := db.QueryRowContext(ctx, "SELECT dirty FROM schema_migrations").Scan(&dirty); err != nil || dirty {
		t.Errorf("往復後の dirty = %v, err = %v", dirty, err)
	}
}

// 定義書(docs/time_slot_reservation_api_tables.md)の制約・索引が実DBに入っていること。
func TestSchema_ConstraintsMatchSpec(t *testing.T) {
	db := StartMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	show := func(table string) string {
		var name, ddl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+table).Scan(&name, &ddl); err != nil {
			t.Fatalf("SHOW CREATE TABLE %s: %v", table, err)
		}
		return ddl
	}
	wants := map[string][]string{
		"users": {
			"UNIQUE KEY `uq_users_keycloak_sub` (`keycloak_sub`)",
		},
		"slots": {
			"UNIQUE KEY `uq_slots_public_id` (`public_id`)",
			"KEY `idx_slots_status_start_at` (`status`,`start_at`)",
			"CONSTRAINT `ck_slots_start_before_end` CHECK",
			"CONSTRAINT `ck_slots_deadline_not_after_start` CHECK",
			"CONSTRAINT `ck_slots_capacity_positive` CHECK",
			"CONSTRAINT `ck_slots_status` CHECK",
		},
		"reservations": {
			"UNIQUE KEY `uq_reservations_public_id` (`public_id`)",
			"UNIQUE KEY `uq_reservations_slot_user` (`slot_id`,`user_id`)",
			"KEY `idx_reservations_slot_status` (`slot_id`,`status`)",
			"KEY `idx_reservations_user_status` (`user_id`,`status`)",
			"CONSTRAINT `fk_reservations_slot` FOREIGN KEY (`slot_id`) REFERENCES `slots` (`id`) ON DELETE RESTRICT",
			"CONSTRAINT `fk_reservations_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE RESTRICT",
			"CONSTRAINT `ck_reservations_status` CHECK",
		},
		"reservation_histories": {
			"KEY `idx_reservation_histories_reservation` (`reservation_id`)",
			"CONSTRAINT `fk_reservation_histories_reservation` FOREIGN KEY (`reservation_id`) REFERENCES `reservations` (`id`) ON DELETE RESTRICT",
			"CONSTRAINT `fk_reservation_histories_actor` FOREIGN KEY (`actor_user_id`) REFERENCES `users` (`id`) ON DELETE RESTRICT",
			"CONSTRAINT `ck_reservation_histories_from_status` CHECK",
			"CONSTRAINT `ck_reservation_histories_to_status` CHECK",
		},
	}
	for table, fragments := range wants {
		ddl := show(table)
		for _, f := range fragments {
			if !strings.Contains(ddl, f) {
				t.Errorf("%s の定義に %q が無い\n%s", table, f, ddl)
			}
		}
	}
}

// 制約に反する INSERT が実DBで失敗すること。各ケースの直前に、対照として正しい行が入ることも見る。
func TestSchema_ViolationsAreRejected(t *testing.T) {
	db := StartMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const ts = "2026-01-01 00:00:00"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("対照の INSERT が失敗した(%s): %v", q, err)
		}
	}
	mustExec("INSERT INTO users (keycloak_sub, created_at, updated_at) VALUES ('sub-1', ?, ?)", ts, ts)
	mustExec(`INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-000000000001', 't', 1, '2026-02-01 09:00:00', '2026-02-01 10:00:00', '2026-02-01 09:00:00', 'published', ?, ?)`, ts, ts)
	mustExec(`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-0000000000a1', 1, 1, 'active', ?, ?)`, ts, ts)

	const (
		errDupEntry   = 1062
		errNoReferred = 1452 // 子行を足せぬ(親が無い)
		errCheck      = 3819
	)
	cases := []struct {
		name string
		want uint16
		q    string
		args []any
	}{
		{"reservations (slot_id,user_id) の重複", errDupEntry,
			`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-0000000000a2', 1, 1, 'active', ?, ?)`, []any{ts, ts}},
		{"reservations.slot_id が存在しない(FK)", errNoReferred,
			`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-0000000000a3', 999, 1, 'active', ?, ?)`, []any{ts, ts}},
		{"reservations.user_id が存在しない(FK)", errNoReferred,
			`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-0000000000a4', 1, 999, 'active', ?, ?)`, []any{ts, ts}},
		{"reservations.status が列挙外(CHECK)", errCheck,
			`INSERT INTO reservations (public_id, slot_id, user_id, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-0000000000a5', 1, 1, 'bogus', ?, ?)`, []any{ts, ts}},
		{"slots start_at >= end_at(CHECK)", errCheck,
			`INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-000000000002', 't', 1, '2026-02-01 10:00:00', '2026-02-01 09:00:00', '2026-02-01 09:00:00', 'draft', ?, ?)`, []any{ts, ts}},
		{"slots reservation_deadline_at > start_at(CHECK)", errCheck,
			`INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-000000000003', 't', 1, '2026-02-01 09:00:00', '2026-02-01 10:00:00', '2026-02-01 09:30:00', 'draft', ?, ?)`, []any{ts, ts}},
		{"slots capacity = 0(CHECK)", errCheck,
			`INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES ('00000000-0000-4000-8000-000000000004', 't', 0, '2026-02-01 09:00:00', '2026-02-01 10:00:00', '2026-02-01 09:00:00', 'draft', ?, ?)`, []any{ts, ts}},
		{"reservation_histories.actor_user_id が存在しない(FK)", errNoReferred,
			`INSERT INTO reservation_histories (reservation_id, from_status, to_status, actor_user_id, occurred_at)
VALUES (1, NULL, 'active', 999, ?)`, []any{ts}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, c.q, c.args...)
			var myErr *mysql.MySQLError
			if !errors.As(err, &myErr) || myErr.Number != c.want {
				t.Errorf("want MySQL error %d, got %v", c.want, err)
			}
		})
	}

	// ON DELETE RESTRICT:予約が入った枠は削除できない。
	_, err := db.ExecContext(ctx, "DELETE FROM slots WHERE id = 1")
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) || myErr.Number != 1451 {
		t.Errorf("予約のある枠の DELETE が FK(1451)で拒まれなかった: %v", err)
	}
}
