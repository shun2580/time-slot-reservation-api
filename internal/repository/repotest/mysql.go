// Package repotest は repository の試験が共有する土台を置く。
// testcontainers-go で MySQL を立て、migrations/ を全て当てた *sql.DB を渡す。
//
// 設計:試験ごとに新規コンテナを立てる(StartMySQL)。試験どうしが状態を共有せず、
// 実行順や並列実行に依らぬことを優先した。起動に十数秒かかるため、
// 1つの試験の中で場面を分けたいときは Reset で表を空に戻して使い回す。
package repotest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql" // mysql:// の database ソース
	_ "github.com/golang-migrate/migrate/v4/source/file"    // file:// の migration ソース
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"

	_ "github.com/go-sql-driver/mysql" // database/sql の "mysql" ドライバ
)

const (
	// mysqlImage は docker-compose.yml の mysql サービスと同じ版にそろえる。
	// 食い違いは TestMySQLImageMatchesCompose が検出する。
	mysqlImage = "mysql:8.4.7"

	// 使い捨てコンテナ専用の値。秘密ではない。
	dbName     = "slotres_test"
	dbUser     = "test"
	dbPassword = "test"

	startTimeout = 3 * time.Minute
)

// resetTables は Reset が空にする表。FK で参照される側(users・slots)が後になる順に並べる。
var resetTables = []string{"reservation_histories", "reservations", "slots", "users"}

// RepoRoot は REPO のルート(go.mod のある場所)を返す。
// この .go ファイルの位置から解決するので、試験の実行ディレクトリに依らない。
// (-trimpath でビルドした試験では runtime.Caller の位置が変わり、解決できなくなる。)
func RepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("repotest: runtime.Caller が失敗した")
	}
	// <root>/internal/repository/repotest/mysql.go
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// MigrationsDir は migrations/ の絶対パスを返す。
func MigrationsDir() string {
	return filepath.Join(RepoRoot(), "migrations")
}

// StartMySQL は MySQL のコンテナを立て、migrations/ を全て当てた *sql.DB を返す。
// 時刻は JST にそろえる(コンテナ TZ=Asia/Tokyo、--default-time-zone=+09:00、
// 接続は parseTime=true&loc=Asia/Tokyo)。コンテナは t.Cleanup で止める。
// Docker が使えぬときは Skip せず t.Fatal で失敗させる。
func StartMySQL(t testing.TB) *sql.DB {
	t.Helper()
	db, _ := startMySQL(t)
	return db
}

// startMySQL は StartMySQL の本体。試験内で migrate を直接操る場合のために DSN も返す。
func startMySQL(t testing.TB) (*sql.DB, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	ctr, err := tcmysql.Run(ctx, mysqlImage,
		tcmysql.WithDatabase(dbName),
		tcmysql.WithUsername(dbUser),
		tcmysql.WithPassword(dbPassword),
		testcontainers.WithEnv(map[string]string{"TZ": "Asia/Tokyo"}),
		testcontainers.CustomizeRequestOption(func(req *testcontainers.GenericContainerRequest) error {
			req.Cmd = []string{
				"--default-time-zone=+09:00",
				"--character-set-server=utf8mb4",
				"--collation-server=utf8mb4_0900_ai_ci",
			}
			return nil
		}),
	)
	// 失敗しても container が返ることがある。先に止める手配をしてから失敗を見る。
	if ctr != nil {
		t.Cleanup(func() {
			stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := testcontainers.TerminateContainer(ctr, testcontainers.StopContext(stopCtx)); err != nil {
				t.Logf("repotest: コンテナの停止に失敗: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatalf("repotest: MySQL コンテナを起動できない(Docker が使えるか確かめよ): %v", err)
	}

	dsn, err := ctr.ConnectionString(ctx, "parseTime=true", "loc=Asia%2FTokyo")
	if err != nil {
		t.Fatalf("repotest: 接続文字列を作れない: %v", err)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("repotest: sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	waitPing(t, ctx, db)
	migrateUp(t, dsn)
	return db, dsn
}

// waitPing は DB が接続を受けるまで待つ。起動ログの検知と実際の受付開始の間に隙があり得るため。
func waitPing(t testing.TB, ctx context.Context, db *sql.DB) {
	t.Helper()
	var err error
	for {
		if err = db.PingContext(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("repotest: MySQL に接続できない: %v (最後のエラー: %v)", ctx.Err(), err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// newMigrate は migrations/ を file ソースとする migrate を作る。呼び手が Close する。
// migrate は自前の接続を使うので、Close しても試験に渡す *sql.DB は巻き込まれない。
func newMigrate(t testing.TB, dsn string) *migrate.Migrate {
	t.Helper()
	// compose の migrate サービスと同じく multiStatements=true で当てる(dsn には既に ? 以降がある)。
	m, err := migrate.New("file://"+filepath.ToSlash(MigrationsDir()), "mysql://"+dsn+"&multiStatements=true")
	if err != nil {
		t.Fatalf("repotest: migrate.New: %v", err)
	}
	return m
}

func closeMigrate(t testing.TB, m *migrate.Migrate) {
	t.Helper()
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Logf("repotest: migrate.Close: source=%v database=%v", srcErr, dbErr)
	}
}

// migrateUp は migrations/ を全て当てる。
func migrateUp(t testing.TB, dsn string) {
	t.Helper()
	m := newMigrate(t, dsn)
	defer closeMigrate(t, m)
	if err := m.Up(); err != nil {
		t.Fatalf("repotest: migrate up: %v", err)
	}
}

// Reset は4表を空にし、AUTO_INCREMENT も戻す。同じコンテナを使い回す試験の各場面の前に呼ぶ。
// schema_migrations には触れない。
func Reset(t testing.TB, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// FOREIGN_KEY_CHECKS はセッション変数。同じ接続で切り替えて戻すため、接続を1本固定する。
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("repotest: Reset: 接続を得られない: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("repotest: Reset: FK 検査を止められない: %v", err)
	}
	for _, table := range resetTables {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s", table)); err != nil {
			t.Fatalf("repotest: Reset: TRUNCATE %s: %v", table, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("repotest: Reset: FK 検査を戻せない: %v", err)
	}
}
