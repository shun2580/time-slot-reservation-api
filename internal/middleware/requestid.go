// Package middleware は、全ルートに共通する横断処理(リクエストID・アクセスログ・panic の回収)を置く。
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// RequestIDHeader は、リクエストIDを受け取り・返すヘッダ名。
const RequestIDHeader = "X-Request-ID"

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// 受け入れるリクエストIDは、英数字とハイフンの 1〜64 文字。
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// RequestID は、X-Request-ID が妥当ならそれを採用し、そうでなければ生成して、
// レスポンスヘッダと context の両方に載せる。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom は、context に入ったリクエストIDを返す。無ければ空文字。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// newRequestID は、crypto/rand の16バイトを16進にした32文字を返す。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 乱数源が読めない異常時でも、リクエストは止めない。時刻由来の値で代える。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
