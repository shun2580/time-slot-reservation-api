package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

// SlotRepository は slots 表への永続化を担う。
// db が *sql.DB なら通常の、*sql.Tx ならそのトランザクションの中の操作になる(同じ SQL を使い回す)。
// usecase.SlotStore(db を渡したとき)と usecase.SlotTx(tx を渡したとき)の両方を満たす。
//
// reservations 表は読み取り(件数・存在)だけ。書き込みは殿が書く ReservationRepository の持分。
type SlotRepository struct {
	db DBTX
}

// NewSlotRepository は SlotRepository を作る。
func NewSlotRepository(db DBTX) *SlotRepository { return &SlotRepository{db: db} }

// slotColumns は slots の全列(s. 接頭辞つき)。scanSlot の順序と一致させる。
const slotColumns = `s.id, s.public_id, s.title, s.capacity, s.start_at, s.end_at, s.reservation_deadline_at, s.status, s.created_at, s.updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanSlot(row rowScanner, extra ...any) (entity.Slot, error) {
	var s entity.Slot
	var status string
	dest := append([]any{&s.ID, &s.PublicID, &s.Title, &s.Capacity, &s.StartAt, &s.EndAt, &s.ReservationDeadlineAt, &status, &s.CreatedAt, &s.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return entity.Slot{}, err
	}
	s.Status = entity.SlotStatus(status)
	return s, nil
}

// Insert は枠を1行入れる。
func (r *SlotRepository) Insert(ctx context.Context, s entity.Slot) (entity.Slot, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO slots (public_id, title, capacity, start_at, end_at, reservation_deadline_at, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.PublicID, s.Title, s.Capacity, s.StartAt, s.EndAt, s.ReservationDeadlineAt, string(s.Status), s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return entity.Slot{}, fmt.Errorf("slots への挿入に失敗: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return entity.Slot{}, fmt.Errorf("slots の id を得られない: %w", err)
	}
	s.ID = id
	return s, nil
}

// remainingSelect は、枠と「有効な予約の数」を LEFT JOIN の集計 1 本で読む(枠ごとの追加クエリ=N+1 にしない)。
// s.id は主キーなので、GROUP BY s.id で s の他の列も選べる(ONLY_FULL_GROUP_BY に通る)。
const remainingSelect = `SELECT ` + slotColumns + `, COUNT(r.id) AS active_count
FROM slots s
LEFT JOIN reservations r ON r.slot_id = s.id AND r.status = 'active'`

func withRemaining(s entity.Slot, active int) entity.SlotWithRemaining {
	return entity.SlotWithRemaining{Slot: s, Remaining: s.Capacity - active}
}

// GetWithRemaining は public_id の枠を残席数つきで返す。
func (r *SlotRepository) GetWithRemaining(ctx context.Context, publicID string) (entity.SlotWithRemaining, error) {
	var active int
	s, err := scanSlot(r.db.QueryRowContext(ctx, remainingSelect+` WHERE s.public_id = ? GROUP BY s.id`, publicID), &active)
	if errors.Is(err, sql.ErrNoRows) {
		return entity.SlotWithRemaining{}, usecase.ErrNotFound
	}
	if err != nil {
		return entity.SlotWithRemaining{}, fmt.Errorf("slots の取得に失敗: %w", err)
	}
	return withRemaining(s, active), nil
}

// List は条件に合う枠を start_at, id の昇順で返す。
func (r *SlotRepository) List(ctx context.Context, q usecase.SlotListQuery) ([]entity.SlotWithRemaining, error) {
	var where []string
	var args []any
	if q.ReservableAt != nil {
		where = append(where, "s.status = ? AND s.reservation_deadline_at > ?")
		args = append(args, string(entity.SlotPublished), *q.ReservableAt)
	}
	if q.Status != nil {
		where = append(where, "s.status = ?")
		args = append(args, string(*q.Status))
	}
	if q.From != nil {
		where = append(where, "s.start_at >= ?")
		args = append(args, *q.From)
	}
	if q.To != nil {
		where = append(where, "s.start_at < ?")
		args = append(args, *q.To)
	}

	query := remainingSelect
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += "\nGROUP BY s.id"
	if q.AvailableOnly {
		// 引き算(unsigned の capacity − count)は負で範囲外エラーになりうるので、比較で書く。
		query += "\nHAVING COUNT(r.id) < s.capacity"
	}
	query += "\nORDER BY s.start_at ASC, s.id ASC\nLIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("slots の一覧に失敗: %w", err)
	}
	defer rows.Close()

	items := []entity.SlotWithRemaining{}
	for rows.Next() {
		var active int
		s, err := scanSlot(rows, &active)
		if err != nil {
			return nil, fmt.Errorf("slots の読取に失敗: %w", err)
		}
		items = append(items, withRemaining(s, active))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("slots の読取に失敗: %w", err)
	}
	return items, nil
}

// LockByPublicID は枠の行を FOR UPDATE で取る。トランザクションの中で、最初に呼ぶこと。
func (r *SlotRepository) LockByPublicID(ctx context.Context, publicID string) (usecase.LockedSlot, error) {
	s, err := scanSlot(r.db.QueryRowContext(ctx, `SELECT `+slotColumns+` FROM slots s WHERE s.public_id = ? FOR UPDATE`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return usecase.LockedSlot{}, usecase.ErrNotFound
	}
	if err != nil {
		return usecase.LockedSlot{}, fmt.Errorf("slots のロックに失敗: %w", err)
	}
	return usecase.LockedSlot{Slot: s}, nil
}

// HasAnyReservation は、取消済みを含む全状態の予約が1つでもあるか(R1)。
func (r *SlotRepository) HasAnyReservation(ctx context.Context, s usecase.LockedSlot) (bool, error) {
	return r.exists(ctx, `SELECT EXISTS(SELECT 1 FROM reservations WHERE slot_id = ?)`, s.Slot.ID)
}

// HasActiveReservation は、有効な予約が1つでもあるか。
func (r *SlotRepository) HasActiveReservation(ctx context.Context, s usecase.LockedSlot) (bool, error) {
	return r.exists(ctx, `SELECT EXISTS(SELECT 1 FROM reservations WHERE slot_id = ? AND status = 'active')`, s.Slot.ID)
}

func (r *SlotRepository) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var ok bool
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&ok); err != nil {
		return false, fmt.Errorf("reservations の存在確認に失敗: %w", err)
	}
	return ok, nil
}

// UpdateFields は編集できる列と updated_at を書く。
func (r *SlotRepository) UpdateFields(ctx context.Context, s usecase.LockedSlot, to entity.Slot) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE slots SET title = ?, capacity = ?, start_at = ?, end_at = ?, reservation_deadline_at = ?, updated_at = ? WHERE id = ?`,
		to.Title, to.Capacity, to.StartAt, to.EndAt, to.ReservationDeadlineAt, to.UpdatedAt, s.Slot.ID)
	if err != nil {
		return fmt.Errorf("slots の更新に失敗: %w", err)
	}
	return nil
}

// UpdateStatus は status と updated_at を書く。
func (r *SlotRepository) UpdateStatus(ctx context.Context, s usecase.LockedSlot, to entity.SlotStatus, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE slots SET status = ?, updated_at = ? WHERE id = ?`, string(to), at, s.Slot.ID)
	if err != nil {
		return fmt.Errorf("slots の状態更新に失敗: %w", err)
	}
	return nil
}

// Delete は枠を物理削除する。
func (r *SlotRepository) Delete(ctx context.Context, s usecase.LockedSlot) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM slots WHERE id = ?`, s.Slot.ID); err != nil {
		return fmt.Errorf("slots の削除に失敗: %w", err)
	}
	return nil
}

var (
	_ usecase.SlotStore = (*SlotRepository)(nil)
	_ usecase.SlotTx    = (*SlotRepository)(nil)
	_ usecase.TxRunner  = (*TxManager)(nil)
	_ DBTX              = (*sql.DB)(nil)
	_ DBTX              = (*sql.Tx)(nil)
)
