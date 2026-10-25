package entity

import (
	"regexp"
	"testing"
)

// UUID v4 の形: 8-4-4-4-12 の小文字16進で、版は 4、variant は 8/9/a/b。
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewPublicID_IsLowercaseUUIDv4(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewPublicID()
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("NewPublicID() = %q, UUID v4 の形ではない", id)
		}
	}
}

func TestNewPublicID_IsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewPublicID()
		if _, dup := seen[id]; dup {
			t.Fatalf("NewPublicID() が重複した: %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewPublicID_PassesParsePublicID(t *testing.T) {
	id := NewPublicID()
	got, ok := ParsePublicID(id)
	if !ok || got != id {
		t.Fatalf("ParsePublicID(%q) = (%q, %v), 自分で採番した ID はそのまま通るはず", id, got, ok)
	}
}

func TestParsePublicID(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"小文字の UUID", "123e4567-e89b-42d3-a456-426614174000", "123e4567-e89b-42d3-a456-426614174000", true},
		{"大文字は小文字にそろえる", "123E4567-E89B-42D3-A456-426614174000", "123e4567-e89b-42d3-a456-426614174000", true},
		{"版は見ない(v1 でも形が合えば通す)", "123e4567-e89b-12d3-a456-426614174000", "123e4567-e89b-12d3-a456-426614174000", true},
		{"空文字", "", "", false},
		{"ハイフン無し", "123e4567e89b42d3a456426614174000", "", false},
		{"短い", "123e4567-e89b-42d3-a456-42661417400", "", false},
		{"長い", "123e4567-e89b-42d3-a456-4266141740000", "", false},
		{"16進でない文字", "123e4567-e89b-42d3-a456-42661417400g", "", false},
		{"前後の空白", " 123e4567-e89b-42d3-a456-426614174000", "", false},
		{"末尾の改行", "123e4567-e89b-42d3-a456-426614174000\n", "", false},
		{"数値の ID", "42", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParsePublicID(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ParsePublicID(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
