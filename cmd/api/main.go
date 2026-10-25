// Command api は、枠予約APIのサーバーを起動する。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // コンテナに tzdata が無くても Asia/Tokyo を引けるようにする

	"github.com/shun2580/time-slot-reservation-api/internal/config"
	"github.com/shun2580/time-slot-reservation-api/internal/handler"
	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
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

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           buildHandler(handler.NewRouter(), logger),
		ReadHeaderTimeout: readHeaderTimeout,
	}

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

// buildHandler は、共通ミドルウェアを RequestID → Logging → Recover の順(外側から)で inner に被せる。
func buildHandler(inner http.Handler, logger *slog.Logger) http.Handler {
	h := middleware.Recover(handler.InternalServerError)(inner)
	h = middleware.Logging(logger)(h)
	return middleware.RequestID(h)
}
