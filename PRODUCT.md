# Syntoria Reach — Product Definition

- Status: **設計方針確定 / 実装前**
- Updated: 2026-11-01
- Initial OS: **Linux**
- Connection architecture: **B 案 — Cloudflare Workers + Durable Objects + Linux Connector**

## 1. プロダクトの定義

**Give your AI the ability to act in your environment.**

Syntoria Reach は、ChatGPT、Claude、Codex、その他の AI が、ユーザーの所有・管理する Linux PC / サーバーの機能を安全に利用するための独立したサービスである。AI は標準 MCP を通じてファイルを操作し、コマンドを実行し、長時間の処理を Job として管理できる。

AI が判断・計画する。Reach は接続、アクセス制御、操作、実行状態、結果と監査情報を提供する。Reach に LLM を内蔵することは基本要件としない。

**初期の本格利用サービスは DaVinci とする。** DaVinci が開発工程や Agent を指揮し、Reach がその Linux 実行基盤を担う。Reach は DaVinci の内部コンポーネントではなく、他の AI も単体利用できる独立サービスである。

## 2. 人間に提供する価値

| 利用場面 | ユーザーが得られること |
| --- | --- |
| 外出先からの依頼 | スマートフォンの AI に依頼し、自分の Linux 開発環境で調査・ビルド・テストを行える |
| 既存環境の活用 | リポジトリ、ファイル、ツール、プライベート環境を移設せず AI に使わせられる |
| AI の交換 | AI を ChatGPT から別の MCP クライアントへ変えても実行環境を再利用できる |
| 長時間作業 | 接続が切れても実行済みの Job を失わず、後から状態・ログ・結果を照会できる |
| 自律開発 | DaVinci が複数の Agent・ビルド・検証を Reach の実行環境上で安全に進め、開発工程は DaVinci のまま管理できる |
| 安全な委譲 | AI ごとに操作対象・作業領域・権限を設定し、操作内容を追跡できる |

目標は、ユーザーが自分の環境をリモート操作する手間と、AI に作業させるための環境移設・再設定の手間を減らすことである。

## 3. 利用体験

### ケース A：ファイルとコマンド

1. ユーザーが Linux 上で Reach を設定し、Workspace と許可する操作を選択する。
2. AI が Reach の MCP Tool から対象 Node・Workspace・利用可能な Capability を確認する。
3. AI は許可されたファイルを読み、必要なコマンドを実行する。
4. Reach は操作結果を返し、実行元・対象・結果を記録する。
5. 必要に応じて AI が次の判断・操作を行う。

### ケース B：非同期 Job

1. AI がビルドやテストを Job として開始する。
2. Reach が要求の一意性を確認し、受理した Job をローカルに記録する。
3. AI が終了・切断しても、Linux 側で受理済み Job の処理を継続する。
4. AI は再接続後に Job ID から状態、ログ、結果を取得する。
5. 停止要求の受付と、実際に停止できたことを区別して表示する。

### ケース C：Node オフライン

対象 Node がオフラインの場合は利用不可を明示する。MVP では自動的に要求を保留・蓄積し、後から勝手に実行することはしない。

### ケース D：DaVinci の実行基盤

1. DaVinci は Issue の目的から作業を判断し、Workspace の用途と実行する Codex / Claude Code / DSH 等を選ぶ。
2. DaVinci が Reach のローカル API に実行を依頼し、Reach が隔離された Linux プロセスと対話型入出力を用意する。
3. DaVinci の Agent Adapter は元の Agent プロトコルと会話・turn の意味を解釈する。Reach はバイト列、プロセスの生存、出力、Job 状態を扱う。
4. DaVinci は Reach の Process Session / Job ID を自分の Run に結びつけ、進捗を監督し、検証結果を受け取る。
5. DaVinci が再起動した場合も Reach の現状を再取得し、終了不明や入力配送不明を確認してから続行する。元の Issue の完了・PR 提出は DaVinci が判断する。

## 4. 提供する能力

| Capability | MVP | 説明 |
| --- | --- | --- |
| Node discovery | Yes | 利用可能な Node と接続状態 |
| Workspace management | Yes | 許可された操作領域の定義・参照 |
| File access | Yes | 一覧、読み取り、条件付き書き込み |
| Command execution | Yes | 許可された Linux コマンドの隔離実行 |
| Asynchronous jobs | Yes | 開始、状態・出力取得、停止 |
| Interactive process sessions | Yes | 継続的な stdin、stdout/stderr、プロセス状態、再接続・入力所有権管理 |
| Authentication and permissions | Yes | AI クライアント、Node、Workspace、操作単位の許可 |
| Audit | Yes | 操作主体、対象、時刻、結果の記録 |
| Browser / Desktop GUI | Later | ブラウザ・画面の操作 |
| Multi-node | Later | 複数の Linux / 他 OS への拡張 |
| Agent-specific orchestration | Later | Agent ごとの計画や会話管理。インストール済み CLI の汎用プロセス起動は MVP で対応 |
| P2P / WebRTC | Later | 対応クライアント向けの直接通信経路 |
| MCP Events | Later | Job 完了・Node 状態などの非同期通知 |

## 5. コア概念

- **Node**：操作対象のコンピューター。初期は 1 Linux Node。
- **Workspace**：ユーザーが AI に開放するファイル・操作の範囲。
- **Capability**：File、Command、Job など Reach が提供する能力。
- **Client / Connection**：認証済みの AI クライアントと接続。
- **Operation**：個別の操作要求。一意の識別子を持ち、再送・重複を判別する。
- **Job**：通信セッションから独立した非同期実行とその状態。
- **Process Session**：実プロセスに紐づく継続的な stdin/stdout/stderr の操作と再接続の識別子。MCP セッションや LLM の会話セッションとは異なる。
- **Artifact**：出力ログや生成物への参照。
- **Permission**：主体・対象・操作に対する許可の定義。

## 6. サービス境界と独立性

Reach は独立サービスであり、Weave、Lore、Trace、特定の LLM の稼働を必要としない。

| サービス | 責務 |
| --- | --- |
| Lore | 知識・長期記憶 |
| Weave | Action・Collaboration・人間と AI の共有状態・承認 |
| Trace | 活動・会話などの記録 |
| Reach | 実行環境へのアクセスと操作・Job 管理 |

Weave で承認された Action に基づいて Reach を利用する場合は、AI / Orchestrator / Adapter が両サービスを組み合わせる。Reach は Weave の Action ID や承認状態を内部の必須モデルとしない。

Reach の Job が完了したことは、Weave の Action が完了したことと同義ではない。両者の判断・反映は利用元が担当する。

### DaVinci との責務分担

| 項目 | Reach | DaVinci |
| --- | --- | --- |
| Linux の Process / Job | 起動、隔離、状態、停止・再接続・ログ | 指示、結果の解釈 |
| 対話型プロセス | 入出力・実行ライフサイクル・所有権 | Agent 固有の protocol、session・turn |
| Workspace | 許可領域と実行時 mount / パスの制限 | 作業対象、clone・branch・成果の意味と管理 |
| 認可 | Node / Workspace / Capability / Operation の最終強制 | 開発要求・実行目的・担当者の管理 |
| 開発 Run | Job / Process Session の実行事実のみ | Run / Attempt、Issue、Sub Issue、PR、検証・受入れ |
| Cloud 接続 | 汎用 MCP / Node transport | DaVinci 固有の Dashboard、Issue 投影・報告 |

DaVinci の Run と Reach の Job は別の実体であり、1 Run から複数の Reach Job / Process Session を利用できる。DaVinci は Linux の実行を Reach に委譲する利用サービスとなるが、Reach 自体は DaVinci を知らずに動作する。既存 DaVinci の動作環境は検証・移行の完了まで保持する。

[DaVinci 統合方針](docs/davinci-execution-integration.md)

## 7. リモート提供方式（採用決定：B 案）

- **Cloudflare Workers**：Remote MCP の安定した入口、認証・要求検証・ルーティング。
- **Cloudflare Durable Objects**：Node 単位の接続 Broker。Linux Connector との WebSocket を仲介する。
- **Linux Connector / Runtime**：Go 製の単一常駐サービス。外向き WSS で接続し、操作と Job を管理する。
- **Linux SQLite**：Operation、Job、権限、監査、結果参照を永続化する。

クラウド側に常時起動の VM / コンテナを用意しない。Durable Objects は WebSocket Hibernation を利用可能な設計とする。リモートからの接続性を維持するため、Linux の Connector は常駐する。

P2P 接続を作成する MCP Tool は初期仕様に含めない。AI が任意の P2P 接続を直接利用できることを前提とせず、通常の MCP Tools を Gateway から中継する。直接通信は後続の Transport Adapter 候補とする。

## 8. 安全性と利用者の制御

- Node、Workspace、Capability、操作ごとに許可を評価する。
- コマンドは隔離された環境で実行し、ホスト権限・秘密情報・ネットワークへのアクセスを最小化する。
- 認証や操作許可は Reach の実行側で検証する。AI の入力や外部文書は権限の根拠にならない。
- 重要な操作はユーザーによる追加承認を求められるようにする。
- 接続が切れてもコマンドを無制限に再試行しない。
- 操作履歴と結果の確認手段を用意する。

## 9. MVP の対象と完成条件

対象は **systemd を利用できる Linux、1 ユーザー、1 Node**。Linux ディストリビューション・カーネル下限は検証して確定する。

MVP では、クラウドから接続した MCP クライアントが許可された Linux Workspace でファイル操作とコマンド実行を行える。長時間処理は Job 化され、MCP 接続切断後も Linux 側で管理される。Operation ID による二重実行防止、権限外アクセスの拒否、オフライン状態の明示、履歴と結果の再取得ができることを完成条件とする。

**追加の MVP 受入条件：対話型プロセスセッションと DaVinci 実利用。** ローカル接続の DaVinci が Reach 経由で Codex 等の Agent プロセスを起動し、継続的に入力・出力を処理し、停止・再接続・ログを扱えること。既存 DaVinci の Run / 検証 / PR 契約を維持した E2E 検証を行うこと。DaVinci 本番環境の移管完了そのものは、別の段階的な切替検収とする。

## 10. 非目標と評価

初期リリースでは、汎用 AI Agent のオーケストレーション、LLM のホスティング、Weave のワークフロー処理、WebRTC P2P、デスクトップ GUI、オフライン Job の自動実行予約は扱わない。Codex 等の実行プロセスを管理することと、Agent の会話・判断プロトコルを Reach が所有することは区別する。

評価指標は、実行成功率、結果不明率、切断後の Job 照合成功率、重複実行の防止、権限拒否の正確性、ユーザーの手動操作負担、異なる AI クライアント間の互換性とする。

関連資料：[README](README.md) / [Technical Architecture](ARCHITECTURE.md) / [DaVinci Integration](docs/davinci-execution-integration.md)
