## テーブル定義(ドラフト)

### 共通方針
- 主キー:内部は連番(BIGINT)、API で見せる ID は別列の UUID(v4、CHAR(36))
- 状態の列:VARCHAR に CHECK 制約をかけ、取りうる値を DB でも守る
- 日時:DATETIME、全て JST
- 管理者の判定:Keycloak のロール(DB には持たない)

### users
| 列 | 型 | 制約 | 説明 |
|---|---|---|---|
| id | BIGINT | PK | |
| keycloak_sub | VARCHAR(255) | NOT NULL, UNIQUE | Keycloak の利用者ID |
| created_at | DATETIME | NOT NULL | |
| updated_at | DATETIME | NOT NULL | |

- 初回アクセス時に作成する。管理者も同じく作成する(履歴の操作者として参照するため)
- メールアドレスは持たない
- アプリ固有の情報:今は持たない(必要になってから足す)

### slots(枠)
| 列 | 型 | 制約 | 説明 |
|---|---|---|---|
| id | BIGINT | PK | |
| public_id | CHAR(36) | NOT NULL, UNIQUE | API で見せる ID(UUID v4) |
| title | VARCHAR(100) | NOT NULL | 枠の表示名 |
| capacity | INT UNSIGNED | NOT NULL | 定員 |
| start_at | DATETIME | NOT NULL | 開始日時 |
| end_at | DATETIME | NOT NULL | 終了日時 |
| reservation_deadline_at | DATETIME | NOT NULL | 予約締切 |
| status | VARCHAR(20) | NOT NULL, CHECK | draft / published / closed |
| created_at | DATETIME | NOT NULL | |
| updated_at | DATETIME | NOT NULL | |

- 案:CHECK 制約で `start_at < end_at`、`reservation_deadline_at <= start_at`、`capacity > 0` を DB でも守る
- 時間が重なる枠は作成できる
- インデックス案:(status, start_at) 公開中の枠を日付の範囲で探すため

### reservations(予約)
| 列 | 型 | 制約 | 説明 |
|---|---|---|---|
| id | BIGINT | PK | |
| public_id | CHAR(36) | NOT NULL, UNIQUE | API で見せる ID(UUID v4) |
| slot_id | BIGINT | NOT NULL, FK → slots.id | |
| user_id | BIGINT | NOT NULL, FK → users.id | |
| status | VARCHAR(20) | NOT NULL, CHECK | active / cancelled_by_user / cancelled_by_admin |
| created_at | DATETIME | NOT NULL | |
| updated_at | DATETIME | NOT NULL | |

- UNIQUE (slot_id, user_id):同じ利用者の重複予約を防ぐ。再予約は行を有効に戻す UPDATE
- 案:slot_id の外部キーは ON DELETE RESTRICT。これで「予約が一度でも入った枠は削除できない」が DB でも守られる
- インデックス案:(slot_id, status) 有効な予約を数えるため/(user_id, status) 自分の予約一覧のため

### reservation_histories(予約の履歴)
| 列 | 型 | 制約 | 説明 |
|---|---|---|---|
| id | BIGINT | PK | |
| reservation_id | BIGINT | NOT NULL, FK → reservations.id | |
| from_status | VARCHAR(20) | NULL, CHECK | 新規予約のときは NULL |
| to_status | VARCHAR(20) | NOT NULL, CHECK | |
| actor_user_id | BIGINT | NOT NULL, FK → users.id | 操作した人(本人または管理者) |
| occurred_at | DATETIME | NOT NULL | |

- 予約の状態が変わるたびに、同じトランザクションで1行追加する
- インデックス案:(reservation_id)

### notification_outbox(通知待ち)
| 列 | 型 | 制約 | 説明 |
|---|---|---|---|
| id | BIGINT | PK | |
| kind | VARCHAR | NOT NULL | 通知の種類(例:代理取消) |
| recipient_user_id | BIGINT | NOT NULL, FK → users.id | 宛先。アドレスは送信時に Keycloak から取得 |
| reservation_id | BIGINT | NOT NULL, FK → reservations.id | 本文は送信時に組み立てる |
| status | VARCHAR(20) | NOT NULL, CHECK | pending / sending / sent / failed |
| attempt_count | INT | NOT NULL | 試行回数(初回を含む。最大4) |
| next_attempt_at | DATETIME | NOT NULL | 次に送る時刻 |
| locked_at | DATETIME | NULL | 送信中にした時刻。10分以上経った行を止まったと見なす |
| last_error_kind | VARCHAR(50) | NULL | 最後の失敗の種類(例:SMTPの一時エラー、Keycloakに接続できない)。エラー文は保存しない |
| created_at | DATETIME | NOT NULL | |
| updated_at | DATETIME | NOT NULL | |

- インデックス案:(status, next_attempt_at) 送信対象を拾うため/(status, locked_at) 止まった行を見つけるため
- 失敗の詳しい内容はログに出し、メールアドレスは伏せる(SMTP のエラー文にはメールアドレスが含まれることがあるため)

## 未決
- なし
