package entity

import (
	"testing"
	"time"
)

func TestJST_IsPlusNine(t *testing.T) {
	utc := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	got := utc.In(JST)

	if got.Hour() != 9 || got.Day() != 25 {
		t.Errorf("UTC 00:00 を JST にすると 25日 09:00 のはず: %v", got)
	}
	if _, off := got.Zone(); off != 9*60*60 {
		t.Errorf("オフセット = %d 秒, want %d", off, 9*60*60)
	}
	if got.Format(time.RFC3339) != "2026-10-25T09:00:00+09:00" {
		t.Errorf("RFC 3339 = %q", got.Format(time.RFC3339))
	}
	if !got.Equal(utc) {
		t.Errorf("JST に直しても同じ瞬間のはず: %v vs %v", got, utc)
	}
}

func TestJST_CrossesDateBoundary(t *testing.T) {
	// UTC の 15:00 は JST の翌日 00:00。
	got := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC).In(JST)
	if got.Day() != 25 || got.Hour() != 0 {
		t.Errorf("UTC 24日15:00 は JST 25日00:00 のはず: %v", got)
	}
}
