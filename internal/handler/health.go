package handler

import "net/http"

// Healthz は、死活確認。認証なしで 200 {"status":"ok"} を返す。
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
