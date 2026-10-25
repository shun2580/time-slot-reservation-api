package repository

import (
	"context"
	"database/sql"
)

// DBTX は *sql.DB と *sql.Tx の両方が満たす最小の interface。
// repository の構造体はこれを持ち、トランザクションの内外で同じ SQL を使い回す。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
