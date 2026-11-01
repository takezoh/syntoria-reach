# Syntoria Reach

**Give your AI the ability to act in your environment.**

Syntoria Reach は、ChatGPT・Claude・Codex などの AI が、ユーザーの Linux 環境にあるファイル・コマンド・ジョブなどを安全に利用するための独立した MCP サービスです。

AI が判断・計画し、Reach が認証・権限確認・操作実行・結果取得を担当します。Reach 自体に LLM を内蔵する必要はありません。

**DaVinci は Reach の初期の本格的な利用サービスです。** DaVinci が開発の計画、Agent セッション、Issue/PR、検証・成果受入れを担当し、Reach が Linux のプロセス、Sandbox、対話型入出力、非同期 Job を実行・管理します。Reach 単体でも利用でき、DaVinci の稼働は必須ではありません。

## ユーザーができること

- 外出先の AI から、自分の Linux PC や開発サーバーの許可された Workspace にアクセスする
- ファイルを読み書きし、コマンドやビルド・テストを実行する
- 長時間処理を Job として開始し、AI との接続が切れても進行状況や結果を後から確認する
- DaVinci などの利用サービスから、継続的な stdin/stdout を必要とする対話型プロセスを管理する
- 複数の AI クライアントから同じ環境の操作能力を利用する
- アクセス元、Workspace、操作の種類ごとに実行権限を制御する

## 採用構成

~~~mermaid
flowchart TB
    AI["ChatGPT / Claude / Codex / other MCP clients"]
    W["Cloudflare Workers: authenticated MCP Gateway"]
    DO["Durable Objects: per-node connection broker"]
    L["Linux Reach: Go Connector + Runtime"]
    DB[("Linux SQLite: operations / jobs / audit")]
    OS["Workspace / files / sandboxed commands / interactive processes"]
    DV["DaVinci Controller: development and agent orchestration"]
    AI <-->|"MCP Streamable HTTP"| W
    W <--> DO
    DO <-->|"Outbound WSS from Linux"| L
    L <--> DB
    L --> OS
    DV <-->|"Local Reach API (Unix socket)"| L
~~~

**Cloudflare 側に常時起動する VM・コンテナは不要**です。Workers は MCP の受付、Durable Objects は接続仲介を担当します。Linux 側では軽量な Reach プロセスが WebSocket を維持し、操作と Job の状態をローカルに保持します。

Cloudflare への通信が切れても、受理済み Job の実行と状態の正本は Linux 側に残ります。Node がオフラインなら、初期版では依頼をキューにためず利用不可を明示します。

## Syntoria の中での位置づけ

- **Lore**：長期記憶・知識
- **Weave**：人間と AI の共有状態、依頼・承認・結果
- **Trace**：活動・会話の記録
- **Reach**：AI に実行環境の能力を提供する

各サービスは独立して利用できます。Weave の MCP Event を受けて Reach の操作を呼び出す場合は、AI または外部の連携コンポーネントが両者を組み合わせます。

## 初期スコープ

Linux 1ユーザー・1 Node、ファイル操作、許可されたコマンド実行、非同期 Job、**対話型プロセスセッション**、権限管理、操作履歴、リモート MCP 接続を対象とします。DaVinci から Reach を実行基盤として使用するローカル E2E を初期の受入条件に加えます。ブラウザ・GUI 操作、複数 Node、ローカル Agent の起動管理、WebRTC P2P は後続の拡張候補です。

現時点は**プロダクト・技術概要の設計段階**です。ここに記述した機能が実装済みであることを示すものではありません。

- [PRODUCT.md](PRODUCT.md) — プロダクト定義、ユーザー価値、MVP
- [ARCHITECTURE.md](ARCHITECTURE.md) — 技術概要設計、境界、実行・接続・復旧の契約
- [docs/davinci-execution-integration.md](docs/davinci-execution-integration.md) — DaVinci と Reach の責務分離、既存実装の参照、段階的移行と検証
