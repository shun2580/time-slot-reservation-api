package usecase_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/usecase"
)

func TestAllBusinessCodes_TenDistinct(t *testing.T) {
	codes := usecase.AllBusinessCodes()
	if len(codes) != 10 {
		t.Errorf("len = %d, want 10", len(codes))
	}
	seen := map[usecase.Code]bool{}
	for _, c := range codes {
		if c == "" || seen[c] {
			t.Errorf("空または重複した Code: %q", c)
		}
		seen[c] = true
	}
}

func TestBusiness_IsBusinessSurvivesWrapping(t *testing.T) {
	err := usecase.Business(usecase.CodeCapacityExceeded, "定員に達している")
	wrapped := fmt.Errorf("wrap: %w", err)
	if !usecase.IsBusiness(wrapped, usecase.CodeCapacityExceeded) {
		t.Error("包まれても errors.As で取り出せるはず")
	}
	if usecase.IsBusiness(wrapped, usecase.CodeAlreadyReserved) {
		t.Error("別の Code には一致しないはず")
	}
	if usecase.IsBusiness(errors.New("x"), usecase.CodeCapacityExceeded) || usecase.IsBusiness(nil, usecase.CodeCapacityExceeded) {
		t.Error("業務エラーでない error・nil は一致しない")
	}
	if got := err.Error(); got != "capacity-exceeded: 定員に達している" {
		t.Errorf("Error() = %q", got)
	}
}

func TestValidationError_Error(t *testing.T) {
	if got := (&usecase.ValidationError{}).Error(); got == "" {
		t.Error("項目が空でも空文字にしない")
	}
	ve := &usecase.ValidationError{Fields: []usecase.FieldError{{Field: "title", Message: "m"}}}
	if got := ve.Error(); got != "入力の誤り: title: m" {
		t.Errorf("Error() = %q", got)
	}
}

func TestSystemClock_ReturnsCurrentTime(t *testing.T) {
	before := time.Now()
	got := usecase.SystemClock{}.Now()
	if got.Before(before) || time.Since(got) > time.Minute {
		t.Errorf("Now() = %v, 現在時刻のはず", got)
	}
}
