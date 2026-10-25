// Command api は、枠予約APIのサーバーを起動する。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // コンテナに tzdata が無くても Asia/Tokyo を引けるようにする

	"github.com/go-sql-driver/mysql"

	"github.com/shun2580/time-slot-reservation-api/internal/config"
	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/handler"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
	"github.com/shun2580/time-slot-reservation-api/internal/repository"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// http.Server のタイムアウト。遅い・止まった接続でゴルーチンと接続を占有されないための上限。
const (
	readHeaderTimeout = 10 * time.Second  // ヘッダを読み切るまで(slowloris 対策)
	readTimeout       = 10 * time.Second  // 本文を含む要求全体を読み切るまで。本APIの本文は小さなJSONだけ
	writeTimeout      = 30 * time.Second  // 応答を書き切るまで。DB 待ちを含む処理の上限
	idleTimeout       = 120 * time.Second // keep-alive の接続を次の要求まで保つ上限
	shutdownTimeout   = 10 * time.Second
)

// DB 接続プールの設定。MySQL 側の max_connections(既定 151)に収まる範囲で、同時要求に足りる数にしている。
const (
	dbMaxOpenConns    = 25
	dbMaxIdleConns    = 25
	dbConnMaxLifetime = 5 * time.Minute // MySQL 側の wait_timeout より短く、切れた接続を掴ませない
	dbPingTimeout     = 5 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("起動または実行に失敗した", "error", err)
		stop()
		os.Exit(1)
	}
}

// run は、設定を読み、ctx が終わるまでサーバーを動かす。ctx が終わったら graceful shutdown する。
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err // 欠けた鍵名だけを含む(値は含まない)
	}

	// sql.Open は接続しない(最初の問い合わせで接続する)。DB が起動前でも API は立ち上がり、/healthz は DB に依らず答える。
	db, err := sql.Open("mysql", cfg.DBDSN)
	if err != nil {
		return err // DSN の書式の誤りなど。DSN 自体は含まれない
	}
	defer db.Close()
	db.SetMaxOpenConns(dbMaxOpenConns)
	db.SetMaxIdleConns(dbMaxIdleConns)
	db.SetConnMaxLifetime(dbConnMaxLifetime)
	// 起動は止めない。繋がらなければ警告をログに残すだけ。終了時は、ping を止めて終わるのを待ってから db を閉じる。
	pingCtx, stopPing := context.WithCancel(ctx)
	pingDone := make(chan struct{})
	go func() { defer close(pingDone); pingDB(pingCtx, db, logger) }()
	defer func() { stopPing(); <-pingDone }()

	slotSvc := usecase.NewSlotService(repository.NewSlotRepository(db), repository.NewTxManager(db), usecase.SystemClock{}, entity.NewPublicID)
	userSvc := usecase.NewUserService(repository.NewUserRepository(db, nil))
	auth := middleware.NewAuth(middleware.AuthConfig{
		Issuer:   cfg.KeycloakIssuer,
		JWKSURL:  cfg.KeycloakJWKSURL,
		Audience: cfg.JWTAudience,
	}, loggingProvisioner{next: userSvc}, handler.WriteStatusProblem, logger)

	routes, err := buildRoutes(routeDeps{slots: slotSvc, auth: auth.Middleware})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}

	srv := newServer(buildHandler(routes, logger))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("サーバーを起動した", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("停止の合図を受けた。graceful shutdown する")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("サーバーを停止した")
	return nil
}

// newServer は、タイムアウトを設定した http.Server を返す。
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// routeDeps は、buildRoutes に渡す部品。具体的な実装(DB につながる usecase、Keycloak の JWT 検証)を
// 差し替えられるようにして、DB も Keycloak も無しで全ルートの登録を試験できるようにする。
type routeDeps struct {
	slots handler.SlotUsecase             // 本番は *usecase.SlotService
	auth  func(http.Handler) http.Handler // 本番は (*middleware.Auth).Middleware
}

// buildRoutes は、全ルートを登録して http.Handler に組み上げる。
func buildRoutes(d routeDeps) (http.Handler, error) {
	r := handler.NewRouter()
	r.Public("GET /healthz", http.HandlerFunc(handler.Healthz))
	r.SetAuth(d.auth)
	handler.RegisterSlotRoutes(r, handler.NewSlotHandler(d.slots))
	// 予約系5本は 501 を返す登録だけ。予約を実装するときは、同じ pattern を二重に登録するとエラーになるので、
	// この呼び出しを先に消してから、実処理の登録に置き換えること。
	handler.RegisterReservationStubs(r)
	return r.Handler()
}

// pingDB は、DB に繋がるかを1回確かめて、結果をログに出す。起動の成否には関わらない。
// 失敗の理由は DSN(パスワードを含む)を含み得るので、エラー文は出さず種別だけを出す。
func pingDB(ctx context.Context, db *sql.DB, logger *slog.Logger) {
	pingCtx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		if ctx.Err() != nil {
			return // 起動直後に停止された。警告にしない
		}
		logger.Warn("DB に繋がらない(最初の問い合わせで再び接続を試みる)", "cause", errorKind(err))
		return
	}
	logger.Info("DB に接続できた")
}

// loggingProvisioner は、利用者の用意(EnsureUser)が失敗した原因をログに残してから、元のエラーをそのまま返す。
// 認証ミドルウェアは失敗を 500 にするだけで原因を出さないので、ここで補う。
// エラー文には sub(利用者の識別子)や SQL の断片が入り得るため、出すのは種別だけにする。
type loggingProvisioner struct {
	next middleware.UserProvisioner
}

func (p loggingProvisioner) EnsureUser(ctx context.Context, keycloakSub string) (int64, error) {
	id, err := p.next.EnsureUser(ctx, keycloakSub)
	if err != nil {
		middleware.LoggerFrom(ctx).Error("利用者の用意に失敗した", "cause", errorKind(err))
	}
	return id, err
}

// errorKind は、エラーを、ログに出してよい短い種別に分ける。エラー文そのものは返さない。
func errorKind(err error) string {
	var myErr *mysql.MySQLError
	switch {
	case errors.Is(err, usecase.ErrEmptyKeycloakSub):
		return "empty_sub"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &myErr):
		return fmt.Sprintf("mysql_error_%d", myErr.Number)
	default:
		return "other"
	}
}

// buildHandler は、共通ミドルウェアを RequestID → Logging → Recover の順(外側から)で inner に被せる。
func buildHandler(inner http.Handler, logger *slog.Logger) http.Handler {
	h := middleware.Recover(handler.InternalServerError)(inner)
	h = middleware.Logging(logger)(h)
	return middleware.RequestID(h)
}
