package handler

import "net/http"

// RegisterReservationStubs は、予約系の5本を Protected で登録し、実処理の代わりに 501 の Problem Details を返す。
// 認証済みなら役割に関係なく 501(未認証は認証ミドルウェアが先に 401)。実処理は工程Dで置き換える。
func RegisterReservationStubs(r *Router) {
	notImplemented := http.HandlerFunc(NotImplemented)
	for _, pattern := range []string{
		"POST /slots/{id}/reservations",
		"GET /reservations",
		"GET /reservations/{id}",
		"POST /reservations/{id}/cancel",
		"GET /slots/{id}/reservations",
	} {
		r.Protected(pattern, notImplemented)
	}
}
