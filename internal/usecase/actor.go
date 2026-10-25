package usecase

// Actor は、操作している人。handler が認証ミドルウェアの Principal から作る(usecase は middleware を知らない)。
type Actor struct {
	UserID  int64  // users.id
	Sub     string // Keycloak の利用者ID
	IsAdmin bool   // Keycloak の admin ロール
}

// RequireAdmin は、管理者専用の操作の入口で呼ぶ。DB を引かずに判定する(R11: 403 は 400・404 より先)。
// handler が本文を読む前に呼んでもよい(認可の定義はここ1か所)。usecase の各メソッドも冒頭で呼ぶ。
func RequireAdmin(a Actor) error {
	if !a.IsAdmin {
		return ErrForbidden
	}
	return nil
}
