package entity

import "time"

// JST は日本標準時。日本は夏時間が無いので固定オフセットで足り、tzdata の有無にも依らない。
// 仕様「時刻」: 時刻は全て JST にそろえる。usecase・handler・repository はこの値を使う。
var JST = time.FixedZone("JST", 9*60*60)
