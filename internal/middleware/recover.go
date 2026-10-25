package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

// Recover は、下流の panic を捕え、request_id つきでログに出してから、onPanic で 500 を返す。
// 500 の本文(Problem Details)の組み立ては onPanic に任せ、この層は handler 層に依存しない。
// 応答をすでに書き始めていたら、onPanic は呼ばない(ステータスを二重に書けないため)。
func Recover(onPanic http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := asRecorder(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v) // net/http が接続を静かに切るための合図。握りつぶさない。
				}
				LoggerFrom(r.Context()).Error("panic recovered",
					"panic", fmt.Sprint(v),
					"stack", string(debug.Stack()),
				)
				if !rec.wroteHeader {
					onPanic(rec, r)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
