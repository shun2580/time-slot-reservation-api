package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// recorder は、ステータスコードと「ヘッダを書いたか」を覚えるラッパー。
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (rec *recorder) WriteHeader(status int) {
	if !rec.wroteHeader {
		rec.status = status
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.status = http.StatusOK
		rec.wroteHeader = true
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap は http.ResponseController が下の ResponseWriter の機能(Flush など)に届くようにする。
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func asRecorder(w http.ResponseWriter) *recorder {
	if rec, ok := w.(*recorder); ok {
		return rec
	}
	return &recorder{ResponseWriter: w, status: http.StatusOK}
}

// Logging は、request_id つきの logger を context に入れ、処理後にアクセスログを1行出す。
// 出す項目は method・path・status・duration_ms・request_id だけ。クエリ文字列・ヘッダ・本文は出さない。
func Logging(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			logger := base.With("request_id", RequestIDFrom(r.Context()))
			ctx := context.WithValue(r.Context(), ctxKeyLogger, logger)

			rec := asRecorder(w)
			next.ServeHTTP(rec, r.WithContext(ctx))

			logger.Info("access",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
			)
		})
	}
}

// LoggerFrom は、context に入った request_id つきの logger を返す。無ければ slog.Default()。
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
