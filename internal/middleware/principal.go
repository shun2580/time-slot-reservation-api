package middleware

import "context"

// Principal は、検証に成功したトークンの持ち主。認証ミドルウェアが context に入れる。
type Principal struct {
	// Sub は、トークンの sub(Keycloak の利用者ID)。
	Sub string
	// UserID は、users テーブルの id。初回アクセスなら作成されたもの。
	UserID int64
	// IsAdmin は、realm_access.roles に "admin" を含むとき真。
	IsAdmin bool
}

// principalKey は、context に Principal を入れる鍵。他の鍵(ctxKey)と型で分ける。
type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom は、context に入った Principal を返す。認証を通っていなければ false。
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
