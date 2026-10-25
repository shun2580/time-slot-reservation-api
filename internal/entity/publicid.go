package entity

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

// NewPublicID は、API で見せる ID(UUID v4)を新しく作る。標準ライブラリだけで足りるので依存を足さない。
func NewPublicID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("entity: 乱数を読めない: %v", err)) // 到達しない(crypto/rand は失敗しない)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var publicIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParsePublicID は、s が UUID の形(8-4-4-4-12 の 16 進)なら小文字にそろえて返す。
// 版(v4)までは見ない: 形が合えば DB を引き、無ければ「存在しない」として 404 になるだけで足りる。
func ParsePublicID(s string) (string, bool) {
	if !publicIDPattern.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}
