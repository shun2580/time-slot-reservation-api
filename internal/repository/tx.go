package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// TxManager は usecase.TxRunner を満たし、トランザクションを張る。
type TxManager struct {
	db *sql.DB
}

// NewTxManager は TxManager を作る。
func NewTxManager(db *sql.DB) *TxManager { return &TxManager{db: db} }

// WithinTx は、READ COMMITTED のトランザクションで fn を実行する。
//
// READ COMMITTED にする理由: 枠を FOR UPDATE でロックした「後」に有効な予約を数える(定員の判定)とき、
// 各文が最新の確定済みの行を読むようにするため。REPEATABLE READ(MySQL の既定)では、
// ロックの前に通常の SELECT を1回でも挟むとスナップショットが古くなり、定員超過をすり抜けうる。
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context, tx usecase.Tx) error) (err error) {
	sqlTx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("トランザクションを開始できない: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = sqlTx.Rollback()
		}
	}()

	if err = fn(ctx, txScope{slots: NewSlotRepository(sqlTx)}); err != nil {
		return err // 包み直さない(errors.As で業務エラーを取り出せるように)
	}
	if err = sqlTx.Commit(); err != nil {
		return fmt.Errorf("コミットできない: %w", err)
	}
	return nil
}

// txScope は usecase.Tx の実装。同じトランザクションに束ねた repository を返す。
// 殿が予約を書くときは、ここへ reservations フィールドと Reservations() を足す。
type txScope struct {
	slots *SlotRepository
}

func (t txScope) Slots() usecase.SlotTx { return t.slots }
