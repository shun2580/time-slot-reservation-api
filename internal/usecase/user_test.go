package usecase

import (
	"context"
	"errors"
	"testing"
)

// fakeUserStore は UserStore の偽物。呼ばれた回数と引数を覚える。
type fakeUserStore struct {
	id    int64
	err   error
	calls int
	gotID string
}

func (f *fakeUserStore) EnsureUser(_ context.Context, keycloakSub string) (int64, error) {
	f.calls++
	f.gotID = keycloakSub
	return f.id, f.err
}

func TestUserService_EnsureUser_ReturnsStoreID(t *testing.T) {
	store := &fakeUserStore{id: 42}
	svc := NewUserService(store)

	got, err := svc.EnsureUser(context.Background(), "sub-abc")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if got != 42 {
		t.Errorf("id = %d, want 42", got)
	}
	if store.calls != 1 || store.gotID != "sub-abc" {
		t.Errorf("store の呼ばれ方 calls=%d sub=%q, want 1 回・sub-abc", store.calls, store.gotID)
	}
}

func TestUserService_EnsureUser_EmptySubIsRejected(t *testing.T) {
	store := &fakeUserStore{id: 42}
	svc := NewUserService(store)

	id, err := svc.EnsureUser(context.Background(), "")
	if !errors.Is(err, ErrEmptyKeycloakSub) {
		t.Errorf("err = %v, want ErrEmptyKeycloakSub", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}
	if store.calls != 0 {
		t.Errorf("空の sub で store が %d 回呼ばれた(0 回であるべき)", store.calls)
	}
}

func TestUserService_EnsureUser_StoreErrorIsPropagated(t *testing.T) {
	boom := errors.New("db down")
	svc := NewUserService(&fakeUserStore{id: 42, err: boom})

	id, err := svc.EnsureUser(context.Background(), "sub-abc")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want store の失敗が errors.Is で辿れること", err)
	}
	if id != 0 {
		t.Errorf("失敗時の id = %d, want 0(store の値を漏らさぬ)", id)
	}
}

// UserService が、認証ミドルウェアの受け取る形(署名)を満たしていること。
func TestUserService_SatisfiesMiddlewareShape(t *testing.T) {
	var _ interface {
		EnsureUser(ctx context.Context, keycloakSub string) (int64, error)
	} = (*UserService)(nil)
}
