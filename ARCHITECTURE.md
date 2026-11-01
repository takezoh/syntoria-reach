# Syntoria Reach — Technical Architecture

- Status: **技術概要設計 / 実装前**
- Updated: 2026-11-01
- Initial runtime: **Linux / Go**
- Adopted remote access: **Cloudflare Workers + Durable Objects + outbound Linux WebSocket (B 案)**

## 1. ゴール・構造原則

Reach は、リモート AI や他のローカルサービスが Linux の能力を標準 MCP またはローカル API で利用する独立した実行サービスである。クラウドに常時起動 VM / コンテナを置かず、Linux 側で Operation / Job / 対話型 Process Session の実行と状態を所有する。

**DaVinci を初期の実利用サービスとする。** DaVinci の開発判断・Agent protocol・GitHub workflow は DaVinci に残し、Linux の process / sandbox / streaming I/O / job execution を Reach に集約する。Reach は DaVinci がなくても動作する。

**アーキテクチャ上の不変条件**

1. Reach は Weave、Lore、Trace、DaVinci、ローカル LLM に依存せず起動・動作できる。
2. MCP は操作契約であり、通信経路と Linux 実行の実装から分離する。
3. クラウドは接続・認証・転送を担い、Job の正本は Linux に置く。
4. 承認済み接続であっても、各操作を Reach Runtime が認可する。
5. リクエストの再送は副作用の二重実行を引き起こさない。
6. 通信断・タイムアウトを実行失敗と断定しない。
7. Node オフライン時の要求は MVP では実行待ちキューに保存しない。
8. 接続先 Linux にインストールするのは軽量 Connector / Runtime であり、LLM を必須としない。
9. プロセスの生存・I/O・Sandbox・Job は Reach の責務とし、DaVinci Run / Agent turn / GitHub Issue の意味判断を取り込まない。
10. 対話型プロセスに接続したクライアントが切断しても、既に受理された実行のライフサイクルを暗黙に変更しない。

## 2. 全体構成

~~~mermaid
flowchart TB
    Clients["ChatGPT / Claude / Codex / other MCP hosts"]
    DaVinci["DaVinci Controller + Agent protocol adapters"]
    subgraph CF["Cloudflare — serverless"]
        GW["Workers: Stateless MCP Gateway / Auth / Routing"]
        BR["Durable Objects: Per-node Connection Broker"]
        GW <--> BR
    end
    subgraph Host["Linux — user-managed"]
        REACH["Reach Daemon (Go): Connector + Operation Service"]
        LOCAL["Local API (Unix socket)"]
        CAP["Capability adapters: File / Command"]
        PROC["Process Session Manager: stdin / stdout / stderr"]
        JOB["Job Manager + OS process supervisor"]
        ST[("SQLite: Operations / Jobs / Sessions / Policy / Audit")]
        LOCAL <--> REACH
        REACH --> CAP
        REACH --> JOB
        REACH --> PROC
        REACH <--> ST
        PROC --> JOB
        JOB --> CAP
    end
    Clients <-->|"MCP Streamable HTTP"| GW
    BR <-->|"Outbound WSS established by Linux"| REACH
    DaVinci <-->|"Local execution API"| LOCAL
~~~

Linux 側は Go の単一 Daemon を標準配置とする。クラウドは常駐コンテナを持たないが、リモートからの無人アクセスには Linux Connector 自体の常駐が必要である。

## 3. コンポーネント・責務

| コンポーネント | 責務 | 保有しないもの |
| --- | --- | --- |
| Workers MCP Gateway | MCP リクエスト、クライアント認証・認可、Node 解決、入力検証、Broker 呼び出し | Linux Job の正本 |
| Durable Object / Node Broker | Node に対する WSS 接続の受付・対応付け、メッセージ配送、短時間の応答相関 | ユーザーのファイル、永続 Job 結果 |
| Reach Connector (Go) | クラウドへのアウトバウンド WSS、認証済み要求受信、再接続、Transport 分離 | LLM の推論 |
| Local API (Unix socket) | DaVinci 等のローカルサービスへ Core と共通の実行操作を提供 | Issue/PR の意味判断 |
| Process Session Manager | 長寿命プロセスの stdin / stdout / stderr、I/O 順序、単一 writer、再接続・停止照合 | Agent 固有の会話 protocol |
| Operation Service | 引数・権限の再検証、冪等性判定、同期・非同期処理の受付 | Syntoria 他サービスの業務状態 |
| Capability Registry | File / Command / Job の公開契約、必要な許可、OS Adapter の選択 | 外部 AI の意思決定 |
| Linux Runtime | Workspace のファイル操作、プロセス起動・停止・隔離 | MCP プロトコル詳細 |
| Job Manager | ジョブの状態、ログ・成果物、停止、再起動後の照合 | MCP 接続の生存状態への依存 |
| SQLite | Operation、Job、Policy、Audit のローカル正本 | 他 Syntoria サービスのデータ |

設計上、MCP Gateway と Transport Broker は別コンポーネントだが、同一 Cloudflare アカウントの Worker / Durable Object binding で一体運用できる。

### Durable Objects / Hibernation の注意点

Durable Object は Node を識別できる安定した ID に対応付ける。Hibernation 中の WebSocket は Cloudflare 側で維持できるが、オブジェクトのメモリは失われる。復帰時は WebSocket attachment 等から接続識別子を再構成する。**メモリ内の map や待機中 Promise を永続状態として扱わない。**

WebSocket の切断や Cloudflare 側の再起動は通常の障害ケースとし、Linux 側が再接続・再登録する。接続先 Node が不在なら Gateway は明示的にオフライン応答を返す。

## 4. ローカル・リモート通信

### 4.0 DaVinci のローカル接続

同じ Linux Node 上の DaVinci Controller は Reach のローカル API を Unix Domain Socket 経由で呼ぶ。要求の DTO と Operation / Job / Process Session の契約はリモート MCP Adapter と共通とし、Cloudflare を経由しない。

ローカル接続も接続元 identity と許可を検証する。Unix Socket の OS 権限だけをグローバルな操作許可の代用にせず、DaVinci 用の権限プロファイルで Workspace、プロセス、Sandbox、必要な credential 経路を限定する。

DaVinci の Agent Adapter は Codex app-server、Claude stream-json、DSH/ACP の protocol を解釈する。Reach は継続的なプロセス I/O とライフサイクルを提供し、LLM の turn 成功や開発成果の妥当性を推定しない。

## 4.1 リモート通信・認証

### 4.2 Node 登録と常時接続

1. Linux 利用者が Node を登録し、明示的に Connection Credential を発行する。
2. Daemon が外向き WSS で Cloudflare に接続する（受信ポートの一般公開は不要）。
3. Gateway / Broker が Credential と Node ID、接続の有効性を検証する。
4. Broker が接続を管理し、接続切断時はオンライン状態を無効にする。
5. Daemon は指数バックオフで再接続し、接続回復後に必要な状態を再登録する。

Credential は保存先の権限を制限し、期限・失効・ローテーションを設計する。Node への WSS 接続だけで任意クライアントのコマンド実行を認めない。

### 4.3 AI からの要求

~~~mermaid
sequenceDiagram
    participant AI as AI / MCP client
    participant GW as Workers MCP Gateway
    participant BR as Durable Object
    participant L as Linux Reach
    participant DB as Local SQLite
    AI->>GW: tools/call with authenticated identity
    GW->>GW: Authorize client + node + tool
    GW->>BR: Route operation with identity context
    BR->>L: Send over registered WSS
    L->>L: Verify policy and input
    L->>DB: Persist operation/admission
    L-->>BR: Accepted operation / job ID
    BR-->>GW: Result
    GW-->>AI: MCP response
    Note over L,DB: Long-running work continues independently
~~~

Gateway は安定した Remote MCP URL を公開する。外部クライアントが WebSocket や WebRTC を直接扱うことを要求しない。MCP のプロトコルライフサイクルと、Reach の明示的な Job / Operation ID は別物とする。

### 4.4 通信の境界

- MCP は HTTP 経由で利用する。Linux Connector の WSS は内部の要求・応答転送路である。
- 接続の生存確認は Job の成功・失敗や処理済み状態の証拠にならない。
- クラウド側の待ち時間は短く制限し、長時間の処理は Linux Job として返す。
- リモート障害時に Gateway が副作用操作を別 ID で再送しない。
- 中継の Message ID / Correlation ID と Operation ID を区別する。
- P2P / WebRTC、専用 Tunnel、VPN は将来の Transport Adapter。MVP は Cloudflare 経由のみで成立する。

## 5. MCP 公開契約（初期案）

初期の MCP Tools は次を候補とする。正式名称と JSON Schema は実装設計で確定する。

| Tool | 目的 | 主な入力 / 結果 |
| --- | --- | --- |
| list_nodes | 認可された Node の照会 | node_id, online / offline |
| list_capabilities | Node の能力の照会 | node_id, capabilities |
| list_workspaces | 許可された Workspace | workspace_id, permission |
| list_files | 許可領域のファイル一覧 | workspace_id, relative_path |
| read_file | ファイル読み取り | workspace_id, relative_path, bounded data |
| write_file | ファイル条件付き書き込み | operation_id, workspace_id, path, expected_version |
| start_job | 非同期コマンド開始 | operation_id, workspace_id, executable, argv, limits |
| start_process_session | 対話型プロセスを Job として開始 | operation_id, workspace_id, executable, argv, sandbox_profile |
| get_process_session | セッション識別子からプロセス・I/O の状態を照会 | process_session_id, lifecycle, output cursors |
| send_process_input | stdin へ継続的に入力を送る | process_session_id, input_id, expected_sequence, bytes |
| read_process_output | stdout / stderr の再取得・増分取得 | process_session_id, stream, cursor, limit |
| stop_process_session | プロセスの停止を要求し終了確認する | operation_id, process_session_id |
| get_operation | 不明な受付結果の照合 | operation_id, admission / final state |
| get_job | Job の現在状態 | job_id, status, timestamps |
| read_job_output | 出力のページ取得 | job_id, stream, cursor, limit |
| cancel_job | Job の停止要求 | operation_id, job_id |

長時間 Job と Process Session の識別子は Linux で生成する。結果を後から参照できるよう、開始・実行状態・出力取得を分離する。MCP Tasks に対応する Host には将来 Adapter を提供可能とし、内部 Job 契約はそれに依存させない。

対話型プロセスツールは、同じ実行 API を使う DaVinci ローカル Client にも提供する。AI Host への細かな Tool の公開方法やストリーム対応は接続 Adapter の問題とし、プロセス・I/O の意味は Core が定義する。

## 6. Operation / Job / 冪等性

**Operation と Job を分ける。** Operation はリクエストの受付・副作用の一意性、Job は継続的な実行の状態である。

副作用要求は Operation ID を必要とする。受信後、認証主体と正規化した引数のハッシュ、受付状態を SQLite にトランザクションで記録してから操作を実行する。

- 同じ主体・Operation ID・引数の再送は既存結果を参照する。
- 同じ Operation ID で引数が異なる場合は conflict を返す。
- 応答が失われた場合は get_operation / get_job で照合する。
- 正体不明の既存操作を新しい ID で再実行しない。
- gateway / DO のメモリ消失は Linux 内の正本を変更しない。

### Job 状態（案）

queued → running → succeeded / failed

running → cancelling → cancelled / failed / unknown

unknown は結果や OS 側状態を確証できない場合に使用する。状態遷移・理由・最終観測時刻を保存する。Stop の要求受付と、プロセス停止済みの事実を区別する。

Job の OS 実行単位は systemd transient unit / cgroups v2 などを検討し、Daemon 再起動時に既存プロセスと照合する。**Linux 自体の再起動で Job が継続する保証は設けない**。再起動後に結果が分からない場合は明示的な unknown とし、自動再実行はしない。

### 対話型 Process Session（DaVinci の初期利用に必須）

- 1 Process Session は Reach が所有する 1 実行プロセスと Job に対応する。DaVinci の Run / Agent session / turn とは別 ID とする。
- 実行開始時は確定した executable / argv、Workspace、Sandbox profile、許可済み環境と必要最小限の credential 参照を指定する。
- stdin は単一の書込み owner を持つ。入力ごとの `input_id` と順序条件により重複や順序違いを拒否する。別の writer が接続する際には所有権の明示的な移譲・fencing を要求する。
- stdout と stderr は分けてローカルに記録する。単調な offset / cursor から上限付きで再取得可能とし、transport の切断により出力欠落・暗黙の再実行が起きないようにする。
- WSS / MCP / DaVinci 側の接続断は detach として扱い、`stop_process_session` による明示的な停止要求とは分離する。
- DaVinci Controller の再起動時は既存 Process Session を照会して再接続し、入力配送の確定結果を確認してから次の turn を送る。
- Reach Daemon が異常終了しても実プロセスが生存することは、OS 管理下の独立 unit / supervisor を実装・検証して初めて保証する。生存が確認できない実行を自動再開しない。
- MVP はパイプ経由のストリーミング I/O を基本候補とし、PTY、FD 受渡し、双方向 backpressure、Agent ごとの handshake は詳細設計・実プロバイダー試験で決める。Agent 固有プロトコルの解析は DaVinci が担う。

## 7. Linux 実行・サンドボックス

初期対象は systemd を利用できる Linux、単一ユーザー・単一 Node。

- Daemon：非 root の systemd user service。必要に応じて linger を明示設定。
- 保存先：ローカル SQLite（WAL）、管理されたログ・成果物ディレクトリ。
- ファイル：Workspace ID と相対パスを受け取り、openat2 などの安全なパス解決で境界を強制する。symlink や mount による逸脱も検査する。
- 書き込み：条件付き更新、必要に応じた一時ファイル / atomic rename。
- コマンド：executable と argv を構造化し、暗黙の shell 展開を行わない。
- 隔離：Linux namespace、bubblewrap、systemd / cgroups v2 等の組み合わせを検証する。
- デフォルト：追加 mount・秘密情報・ネットワーク・特権デバイスへのアクセスなし。DaVinci がモデル API や依存取得を必要とする場合だけ、明示的な Sandbox profile にネットワーク・限定 credential 経路を許可する。
- リソース：CPU、メモリ、プロセス数、実行時間、ログサイズを制限する。

**注意：** bubblewrap や user namespace が存在することだけでは十分な隔離を意味しない。対象ディストリビューション、設定、kernel 機能で有効性を検証する。必要な保護が使えない場合は fail closed とする。特権操作を無制限のコマンド実行で代替しない。

## 8. 認証・認可・監査

### Cloud MCP

外部クライアントの OAuth/OIDC 認証と Reach のユーザー、クライアント、Node の結び付けを検証する。Syntoria Gate を Issuer として利用可能にするが、Reach Core の実行時必須依存にしない。トークンの issuer / audience / expiry / scopes 等を検証し、ユーザーや Node を未検証の要求パラメーターから決めない。

### Linux Runtime

DaVinci ローカル API とクラウドから来た要求の両方で、接続元の認証済み主体、Job / Session の所有者、許可された Workspace / Capability を検証する。認証情報や model token を Remote MCP の任意テキスト・ログに混入させず、必要ならホスト上の専用 FD / credential broker 等の限定経路で渡す。

クラウドから来た要求も、接続元の検証済み identity とサーバーが検証できる認可コンテキストに束縛する。Node、Workspace、Capability、操作・引数に対して Linux 側でも最終認可する。外部文書や LLM 生成文字列は認証・許可にならない。

### 監査

Operation ID、認証された主体、Node、Workspace、操作種別、タイムスタンプ、結果、拒否理由を記録する。秘匿データ、API Key、ファイル内容、環境変数はログに無条件記録しない。ログの保持期間と最大サイズを設定できるようにする。

## 9. 保存データと責任境界

| 保存先 | 内容 | 正本性 |
| --- | --- | --- |
| Linux SQLite | operations, jobs, process_sessions, input_receipts, permissions, audit_records, artifacts の索引 | **Operation / Job / Process Session の正本** |
| Linux files | Job の stdout / stderr、成果物 | **実データの正本** |
| Durable Object | WebSocket 接続識別子、必要最小限の Node 接続メタデータ | 通信制御用。Job の正本にしない |
| Workers | 認証済み要求の一時処理と転送 | 永続 Job 状態は持たない |

データを Cloudflare 側にコピーする要件が生じた場合は、保存対象・秘匿性・保持期間・一貫性の契約を別途設計する。

## 10. 障害時の契約

| 障害 | 期待する動作 |
| --- | --- |
| Linux Node オフライン | node_offline を返す。MVP は自動キューイングしない |
| WebSocket 切断 | Connector がバックオフ再接続。受理済み Job は Linux 側で継続 |
| Cloudflare 一時障害 | 新規操作は不可の場合あり。Linux の Job・結果は保持 |
| DO Hibernation / 再生成 | 接続 metadata を再構成し、必要なら Connector の再登録を要求 |
| HTTP 応答ロスト | outcome_unknown を返し、同一 Operation ID を照会・再送 |
| Daemon クラッシュ | OS の Job / Process Session 状態と SQLite を再照合。確証がない場合 unknown。継続実行は独立プロセス管理の検証結果に依存 |
| DaVinci 再起動 | DaVinci Run が保存した Reach Job / Process Session ID から再接続し、未確認入力・終了を先に照合 |
| 対話型 stdin の配送不明 | input_id / sequence の receipt を照合する。新しい ID で同じ入力を無条件に再送しない |
| 要求の重複 | SQLite の一意制約・引数比較で二重副作用を防止 |
| 権限取り消し | 新規要求を拒否。実行中 Job の扱いは明示した停止ポリシーに従う |

標準エラー分類（案）：node_offline / unauthorized / permission_denied / invalid_request / operation_conflict / result_unknown / sandbox_unavailable / timeout。

可用性の設計目標は「WebSocket が一切切れない」ではなく、**切れても受理済み操作の真実が失われず、再接続で正しく照合できること**である。

## 11. MVP の構築順序

1. **Local Core**：Go Daemon、SQLite、Workspace、File / Command API、Operation ID / 監査。
2. **Linux Execution**：Sandbox、非同期 Job、**対話型 Process Session と継続 I/O**、停止・所有権、再起動後の照合。
3. **DaVinci local integration**：DaVinci 用 Local API / Adapter。既存の Agent protocol と開発 workflow を維持し、実行を Reach に委譲。ローカル E2E を検証。
4. **Remote MCP**：Workers MCP Gateway、OAuth、Node 登録、DO WebSocket Broker、Connector WSS。
5. **Acceptance / rollout**：実 Linux / ホスト済み Cloudflare / 複数 MCP Host の検証と、既存 DaVinci の段階的な切替・復旧試験。

### 必須の受け入れテスト

- Cloudflare を使わず Linux の Reach を単独利用できる。
- 認証された MCP クライアントが許可された Workspace にアクセスできる。
- Workspace 外パス、symlink 逸脱、未許可コマンド、秘密情報の取得を拒否する。
- Job を開始した後に MCP / WSS を切断しても、Linux 上の処理と記録が維持される。
- 同一 Operation ID の再送が二重実行にならない。
- 通信タイムアウトで unknown を正しく扱い、実行結果を照合できる。
- DO の Hibernation / 再生成や Connector 再接続後にジョブ照会できる。
- Node オフライン中に受理していない処理が、復帰後に勝手に実行されない。
- 本番 OAuth、Node credential の失効、permission 変更が反映される。
- DaVinci が Reach 経由で Codex 等を起動し、同じプロセスへ複数回入力し、stdout/stderr を順序通り再取得できる。
- DaVinci の Run / Agent turn / GitHub Issue / draft PR の意味・状態が Reach Job と混同されない。
- DaVinci Controller 再起動・重複 stdin・Session stop・provider credential 分離を実プロセスで検証する。
- Reach の Job ID / Process Session ID と DaVinci の Run ID の対応が DaVinci に保存され、復旧後も照合できる。

## 12. 設計検証が必要な事項

| 論点 | 実装前に検証すること |
| --- | --- |
| Remote MCP | 対象 MCP Host と採用 SDK の 2026-07-28 / 2025-11-25 互換 |
| 認証 | OAuth Issuer / Gate を使う場合の接続登録・失効・権限マッピング |
| Node Credential | 発行、秘密情報保存、期限、失効、再登録 |
| Linux Sandbox | bubblewrap、namespace、cgroups のサポート境界と fail-closed |
| Job | systemd user transient unit / 独立プロセスの復旧と停止 |
| Broker | DO Hibernation 後の WebSocket 復元、同時要求、切断時の相関 |
| MCP 応答 | Tool timeout、Job 受理 ACK、結果不明時の再照合 |
| Data | Operation / Job / Process Session schema、入力 receipt、ログ・artifact 保持量と削除 |
| DaVinci integration | 既存 Run / Agent session / sandbox / credential 契約を維持した Adapter と移行計画 |
| Interactive I/O | stdin の書込み lease、再配送、stdout/stderr cursor、backpressure、PTY の必要性 |

これらは詳細設計および実環境の検証で決める。設計文書は稼働中の機能や本番の受け入れ完了を示すものではない。

## 13. DaVinci の実行環境統合

DaVinci は Reach の初期利用サービスとして、ローカル API を使って Agent プロセス、ビルド、テスト、検証の実行を委譲する。DaVinci の Issue/PR、Agent provider protocol、Run / Attempt、成果の判断・受入れを Reach に移さない。

既存 DaVinci の Go 実装（Process / Sandbox / Command Delivery / Recovery）を設計・テストの参照実装とし、責務単位で Reach に汎用化する。既存コードは DaVinci 固有の依存と終了条件を持つため、一括コピーや旧 Controller の停止を先行させない。

DaVinci が現在持つ Cloud Run / Firestore の Issue 投影、Dashboard、コマンド配送は別プロダクトの業務契約である。Reach の Cloudflare Broker に置き換える対象は汎用実行接続だけであり、DaVinci の業務クラウド機能まで無条件に置換しない。

実装・段階的移行の境界と検収事項は [DaVinci Execution Integration](docs/davinci-execution-integration.md) を参照する。

## 14. 参考仕様

- [Cloudflare: Stateless MCP Handler](https://developers.cloudflare.com/agents/model-context-protocol/apis/handler-api/)
- [Cloudflare: Durable Objects WebSocket Hibernation](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [MCP specification](https://modelcontextprotocol.io/specification/)

関連資料：[README](README.md) / [Product Definition](PRODUCT.md) / [DaVinci Execution Integration](docs/davinci-execution-integration.md)
