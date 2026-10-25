package entity

import "testing"

func TestSlotStatus_Valid(t *testing.T) {
	tests := []struct {
		s    SlotStatus
		want bool
	}{
		{SlotDraft, true},
		{SlotPublished, true},
		{SlotClosed, true},
		{"", false},
		{"Published", false},
		{"cancelled", false},
	}
	for _, tt := range tests {
		if got := tt.s.Valid(); got != tt.want {
			t.Errorf("SlotStatus(%q).Valid() = %v, want %v", tt.s, got, tt.want)
		}
	}
}

func TestReservationStatus_Valid(t *testing.T) {
	tests := []struct {
		s    ReservationStatus
		want bool
	}{
		{ReservationActive, true},
		{ReservationCancelledByUser, true},
		{ReservationCancelledByAdmin, true},
		{"", false},
		{"cancelled", false},
	}
	for _, tt := range tests {
		if got := tt.s.Valid(); got != tt.want {
			t.Errorf("ReservationStatus(%q).Valid() = %v, want %v", tt.s, got, tt.want)
		}
	}
}
