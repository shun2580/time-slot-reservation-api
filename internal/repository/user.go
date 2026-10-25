package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// jst は日本標準時。日本は夏時間が無いので固定オフセットで足り、tzdata の有無にも依らない。
var jst = time.FixedZone("JST", 9*60*60)

// UserRepository は users 表への永続化を担う。
type UserRepository struct {
	db  *sql.DB
	now func() time.Time
}

// NewUserRepository は UserRepository を作る。now は試験で時刻を差し込むためのもので、
// nil なら現在時刻を使う。どちらでも保存する時刻は JST にそろえる。
func NewUserRepository(db *sql.DB, now func() time.Time) *UserRepository {
	if now == nil {
		now = time.Now
	}
	return &UserRepository{db: db, now: now}
}

// EnsureUser は keycloakSub の利用者の id を返す。行が無ければ作る。
//
// 同時に初回アクセスが来ても競合しないよう、SELECT → INSERT の二段にせず、
// 一意制約(uq_users_keycloak_sub)に任せる 1 文で済ませる。
// 既存行のときは ON DUPLICATE KEY UPDATE の LAST_INSERT_ID(id) で、その行の id を LastInsertId に載せる。
// id を自分自身に代入するだけなので、既存行の created_at・updated_at は変わらない。
func (r *UserRepository) EnsureUser(ctx context.Context, keycloakSub string) (int64, error) {
	at := r.now().In(jst)
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO users (keycloak_sub, created_at, updated_at) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
		keycloakSub, at, at)
	if err != nil {
		return 0, fmt.Errorf("users の確保に失敗: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("users の id を得られない: %w", err)
	}
	return id, nil
}
