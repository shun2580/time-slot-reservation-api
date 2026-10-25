package usecase

import (
	"context"
	"errors"
	"fmt"
)

// ErrEmptyKeycloakSub は Keycloak の利用者ID(sub)が空のときに返す。
var ErrEmptyKeycloakSub = errors.New("keycloak の sub が空")

// UserStore は UserService が使う users の保存先。repository.UserRepository が満たす。
type UserStore interface {
	EnsureUser(ctx context.Context, keycloakSub string) (int64, error)
}

// UserService は、初回アクセスで users の行を作る(管理者も同じ)。
type UserService struct {
	store UserStore
}

// NewUserService は UserService を作る。
func NewUserService(store UserStore) *UserService {
	return &UserService{store: store}
}

// EnsureUser は keycloakSub の利用者の内部 id を返す。行が無ければ作る。
// 認証ミドルウェアがこの署名の interface として受け取るので、変えてはならない。
func (s *UserService) EnsureUser(ctx context.Context, keycloakSub string) (int64, error) {
	if keycloakSub == "" {
		return 0, ErrEmptyKeycloakSub
	}
	id, err := s.store.EnsureUser(ctx, keycloakSub)
	if err != nil {
		return 0, fmt.Errorf("利用者を確保できない: %w", err)
	}
	return id, nil
}
