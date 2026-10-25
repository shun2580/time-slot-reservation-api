# 仕様外の案(実装しない・殿の裁量)

工程B各足軽の報告の「ideas」欄から集めた案の一覧。いずれも仕様(docs/time_slot_reservation_api.md)には無く、実装はしていない。採否は殿が決める。仕様の穴(Problem の errors 欠如など)は仕様内の持ち越しゆえここには載せない。

- **Config の DBDSN を JSON 出力から伏せる案** — `Config` を `json.Marshal` すると DBDSN が平文で出る。`json:"-"` を付けるか `MarshalJSON` で伏せる案。
  - 出どころ: subtask_238_B1 (足軽2号)
  - 分類: セキュリティ

- **起動時に APP_DB_PORT の数値検査と APP_HTTP_ADDR の host:port 検査を入れる案** — 現状は「欠け」の検査のみ。
  - 出どころ: subtask_238_B1 (足軽2号)
  - 分類: 運用

- **repository の試験を TestMain で1コンテナ共有する案** — Reset で場面を分ければ試験が速くなる。今回は独立性を優先し未実装。
  - 出どころ: subtask_238_B10 (足軽3号)
  - 分類: 試験

- **api 側でも mysql への接続再試行を持つ案** — compose 外(本番など)でも堅くなる。
  - 出どころ: subtask_238_B34 (足軽4号)
  - 分類: 運用

- **Dockerfile を cmd/ 配下ごとの target にする案** — 第2段階で送信処理や Mailpit を足すとき、ARG で main を切り替える。
  - 出どころ: subtask_238_B34 (足軽4号)
  - 分類: 開発体験

- **compose の起動待ちを `docker compose up --wait --build` で行う案** — CI で healthy を待って戻れる。
  - 出どころ: subtask_238_B34b (足軽4号)
  - 分類: 試験

- **疎通確認スクリプトを scripts/ に置き make 化する案** — 「動かし方」を README で説明しやすくなる。
  - 出どころ: subtask_238_B34b (足軽4号)
  - 分類: 開発体験

- **Problem に request_id を返す案(トレース用)** — 仕様に無いため足していない。
  - 出どころ: subtask_238_B789 (足軽5号)
  - 分類: 運用

- **http.Server の ReadTimeout / WriteTimeout / IdleTimeout の設定案** — 任務に指定が無く ReadHeaderTimeout のみ設定。公開前に要検討。
  - 出どころ: subtask_238_B789 (足軽5号)
  - 分類: 性能

- **operationId の命名規約(動詞+名詞の camelCase)を docs に1行書く案** — 後の handler 名や codegen と揃えやすくなる。
  - 出どころ: subtask_238_A (足軽1号)
  - 分類: 開発体験

- **OpenAPI の lint を CI に入れる場合、redocly.yaml で例外を定める案** — info-license・no-server-example.com・healthz の 4xx を例外にする。
  - 出どころ: subtask_238_A (足軽1号)
  - 分類: 試験
