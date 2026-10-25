package usecase

import (
	"time"

	"github.com/shun2580/time-slot-reservation-api/internal/entity"
)

// Clock は現在時刻の供給元。usecase は time.Now を直接呼ばず、これを使う(試験で固定するため)。
type Clock interface {
	Now() time.Time
}

// SystemClock は実時刻を JST で返す。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().In(entity.JST) }
