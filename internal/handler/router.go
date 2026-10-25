package handler

import "net/http"

// NewRouter は、全ルートを束ねた http.Handler を返す。
// 認証・業務ルートは、工程C・Dがここへ足す。
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", Healthz)
	mux.HandleFunc("/", NotFound) // 未定義パスの受け皿
	return mux
}
