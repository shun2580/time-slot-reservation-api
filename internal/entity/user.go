package entity

import "time"

// User は Keycloak の利用者に対応するアプリ側の行である。
// メールアドレスは持たない(正本は Keycloak)。管理者かどうかも持たない(Keycloak のロールで判定する)。
type User struct {
	ID          int64
	KeycloakSub string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
