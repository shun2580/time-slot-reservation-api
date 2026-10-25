# 枠予約 API

定員制シェアスペース(コワーキングの1日利用など)の時間枠予約 API。Go / MySQL / Docker Compose / Keycloak で作る。

> 第1段階の土台を作成中(下書き)。ここに書かれた機能は、まだ動くものではない。

Go のバックエンド開発力(レイヤー設計・SQL・トランザクション・認可・テスト)を示すポートフォリオとして作っている。定員の判定と同時予約の排他制御が中心。

## ディレクトリ構成と Laravel との対応

PHP / Laravel の経験がある人向けに、Go 側の置き場所を対応づける。

| パス | Laravel での近い物 | 役割 |
|---|---|---|
| `cmd/api` | `public/index.php` と bootstrap | エントリポイント。起動と配線を行う |
| `internal/config` | `config/*.php` と `.env` | 設定の読み込み(値は `.env` から) |
| `internal/entity` | Eloquent の Model | 業務の型。DB を知らない純粋な型 |
| `internal/usecase` | Service・Action クラス | 業務ルールの置場。認可もここで行う |
| `internal/repository` | Eloquent のクエリ / Repository | SQL を手書きする層 |
| `internal/handler` | Controller とルート | リクエストの受付とレスポンスの組み立て |
| `internal/middleware` | Middleware | 認証(JWT 検証)やリクエスト ID の付与 |
| `migrations` | `database/migrations` | スキーマ変更(golang-migrate の SQL ファイル) |
| `docker-compose.yml` | Laravel Sail | API・MySQL・Keycloak などの起動定義 |
| テスト | PHPUnit と `RefreshDatabase` | usecase の単体テスト、testcontainers による使い捨て MySQL |

## 正本

仕様と設計判断は次の3つが正本。実装はこれらと食い違わせない。

- [仕様](docs/time_slot_reservation_api.md)
- [テーブル定義](docs/time_slot_reservation_api_tables.md)
- [OpenAPI 定義](api/openapi.yaml)

## 作る順番

- 第1段階: Docker Compose(API・MySQL・Keycloak)、マイグレーション、OpenAPI、JWT の検証、枠の管理、空き枠の検索、予約・再予約・本人取消・自分の予約一覧、予約の履歴、同時予約の競合テスト
- 第2段階: 代理取消、通知待ちの書き込み、送信処理(別プロセス)、Keycloak 管理APIからの宛先取得、Mailpit

## 起動方法

(後日記す)

## 環境変数

(後日記す)

## 試験の実行方法

(後日記す)
