# DaVinci Execution Integration — Reach 初期利用サービス

- Status: **設計方針 / 実装・移行前**
- Updated: 2026-11-01
- Owners: Reach（汎用 Linux 実行） / DaVinci（自律開発ワークフロー）
- Scope: Linux 1 Node のローカル実行。Remote MCP は Reach の B 案に従う。

## 1. 採用する関係

**DaVinci の実行基盤を Syntoria Reach とする。** DaVinci は GitHub Issue、開発計画、Agent session / turn、検証・PR の責務を保持し、Reach はその実行に必要な Linux プロセス、Sandbox、Workspace 境界、対話型 I/O、Job、ログ、復旧を共通能力として提供する。

Reach は独立したプロダクトであり、DaVinci が停止・未インストールでも ChatGPT 等から単体利用できる。DaVinci は Reach のローカル API を利用する実行クライアントになる。

~~~mermaid
flowchart TB
    Main["DaVinci Controller: Issue / Run / evaluation / acceptance"]
    Adapter["DaVinci Agent adapters: Codex / Claude / DSH"]
    Local["Reach Local API: Unix socket"]
    Runtime["Reach Linux Runtime: process / sandbox / interactive I/O / job"]
    State[("Reach: local SQLite / logs")]
    Main <--> Adapter
    Adapter <-->|"Process Session API"| Local
    Main <-->|"Job / Artifact API"| Local
    Local <--> Runtime
    Runtime <--> State
~~~

DaVinci の Run と Reach の Job / Process Session は別物で、対応表を DaVinci が保存する。1 Run に複数 Job / Process Session が含まれ得る。

## 2. 正本・責務の境界

| Concern | Reach owns | DaVinci owns |
| --- | --- | --- |
| Process | 起動、PID / systemd unit、隔離、終了観測、停止 | どの Agent / Command をいつ実行するか |
| Process Session | stdin の順序と receipt、stdout/stderr の保存・cursor、write lease | Provider protocol（ACP / app-server / stream-json）、LLM turn の文脈と妥当性 |
| Job | 受付、実行ライフサイクル、コード・ログ、停止、再起動後の照合 | Run / Attempt と Job の対応、作業の再計画 |
| Workspace | 操作可能なパスと mount、アクセス強制 | clone / branch / worktree の意味、Git 履歴、提出対象・保持ポリシー |
| Credentials | 限定した実行環境への安全な受渡し・監査 | Provider / GitHub に何の権限が必要かの要求と選択 |
| Artifacts | バイト列・ログ・ハッシュの取得 | 成果・テスト・PR としての意味付け・受入条件 |
| Cloud | 汎用 MCP、Linux Node への経路 | Issue 状態投影、レビュー、Dashboard・開発業務 |

DaVinci の Agent Adapter を Reach に移動しない。Reach に Agent の推論ループ、GitHub Issue モデル、PR 生成や提出判断を組み込まない。

## 3. DaVinci の既存実装（参照先と評価）

以下は 2026-11-01 に確認したソースと設計契約。**これらが Reach に移管・実装済みという意味ではない。**

| 現在の DaVinci ソース | Reach で採る原則 | 変更が必要な点 |
| --- | --- | --- |
| [execution/process_linux.go](https://github.com/takezoh/davinci/blob/main/src/internal/execution/process_linux.go) | Go process、process group、bounded output、private log、終了確認 | 現行の `Pdeathsig: SIGKILL` は DaVinci の Controller 生存に結び付く。Reach では独立 Job / Process Session 生存を別途実現・検証 |
| [sandbox/main.go](https://github.com/takezoh/davinci/blob/main/src/internal/sandbox/main.go) | bubblewrap、namespace、実行 HOME、credential 分離 | DaVinci 固有の launcher mode ではなく Reach Capability ごとの policy / profile に一般化 |
| [recordstore/store.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/recordstore/store.go) | 保存前の意図・atomic snapshot・fsync・再起動照合 | DaVinci の Issue Run 台帳は DaVinci に残す。Reach の実行正本は SQLite / logs |
| [cloudnode/agent.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/cloudnode/agent.go) | Outbound 接続、node credential、heartbeat、重複配送照合 | 現行は HTTPS polling。Reach B 案では Worker / DO と WSS で汎用配送 |
| [cloud/server/commands.go](https://github.com/takezoh/davinci/blob/main/src/internal/cloud/server/commands.go) | Admission、request hash、lease / fence、主体別許可 | Issue 固有の command と repository / run generation を Reach Core へ持ち込まない |
| [DaVinci Architecture](https://github.com/takezoh/davinci/blob/main/ARCHITECTURE.md) | 判断と OS 実行の責務分離、unknown recovery、プロセス停止の証拠 | Reach の Job と DaVinci の Run / Agent session を別の正本として接続 |

移管の判断単位は package 名ではなく、**実行プロセスの所有者・状態の正本・境界の責務**とする。共通化された能力だけを Reach に実装し、DaVinci 固有の workflow は残す。

## 4. 必要な Reach 実行契約

### 4.1 Local API

DaVinci が同じ Linux Node にいる場合は Unix Domain Socket 経由で Reach に接続する。Cloudflare を経由する必要はない。MCP Adapter と同じ Operation / Job / Session の application service と認可を通す。

### 4.2 対話型プロセス

- `start_process_session`：冪等な Operation ID、Workspace、Sandbox profile、executable / argv、リソース制限でプロセスを開始。Reach Job ID と Process Session ID を返す。
- `get_process_session`：実プロセスの生存、exit、I/O cursor、入力所有者と保存状態を取得。
- `send_process_input`：`input_id` と期待順序を用いて stdin へバイト列を渡す。重複入力は再投入せず既存 receipt を返す。
- `read_process_output`：stdout と stderr を独立した増分 cursor で、上限付きに取得する。中断・再接続でも同じ offset から再取得できる。
- `stop_process_session`：停止要求を受付け、OS 側で終了した証拠を照合する。停止要求と停止確認は別の事実。
- `get_job` / `get_operation`：通信断・Daemon 再起動・中断時の状態照合。処理完了不明を勝手に成功/失敗へ変更しない。

シンプルな一回限りのビルド・テストは通常の Job API を使う。継続的な stdin/stdout を必要とする Codex 等は Process Session API を使う。データ形式は provider-agnostic なバイト列と構造化した lifecycle である。

### 4.3 Writer と再接続

1 セッションに対し同時に許可する stdin writer を原則 1 つにする。client / owner / lease / generation を照合し、明示的な移譲前に別の writer が入力を混入できないようにする。リトライは同じ `input_id` と payload を用い、違う内容は競合とする。送達 receipt と Agent turn の解釈を区別する。

stdout/stderr は Reach のローカル保存から cursor で再送できる。転送・ネットワークの disconnect は Process Session の stop を意味しない。ログ保持期限の失効を明示し、失われた出力を成功として扱わない。

### 4.4 実行生存と資格情報

DaVinci の既存 Process Host は Controller 親プロセスの終了に子の終了を結び付ける箇所がある。Reach が DaVinci Controller 再起動中も実プロセスを継続させるには、Reach 管理下の独立した OS 実行単位（systemd transient units 等）が必要。Reach 自身の再起動でも継続できるかは実 Linux で検証する。

Provider の token / refresh credential / GitHub token を公開 MCP の要求本文や stdout に置かない。秘密情報は DaVinci が要求する範囲の host-owned credential reference / FD 等で限定して渡す方式を詳細設計で確定する。DaVinci が持つ現行認証と受入条件を壊さない。

## 5. 移行・統合計画

### Stage 1 — Reach generic execution

Reach Linux Runtime に Sandbox、Job、Process Session、ローカル API、永続 I/O、Operation receipt を実装する。DaVinci のコードは参照・検証基準として利用する。現行 DaVinci は変更せず継続稼働する。

### Stage 2 — DaVinci adapter

DaVinci の Controller / Agent Adapter に Reach 用 execution port を導入する。Run の継続状態と Reach ID の対応を DaVinci の記録に保存する。最初は専用のテスト環境・リポジトリで実装し、既存と同じ Issue / Run / PR 契約を検証する。

### Stage 3 — Local production cutover

既存 Process Host と Reach のどちらが現在の Job / Process Session を所有するかを実行単位で固定し、同一 Run に二重 writer を発生させない。進行中 Run の継続・終了証拠を保持し、切替対象と rollback 手順を明示してから本番適用する。実行途中に異なる runtime に勝手に置換しない。

### Stage 4 — Remote transport consolidation

Reach B 案の Workers + Durable Objects + outbound WSS が E2E 検証された後、DaVinci が必要とする汎用 Node 実行接続を Reach に委譲できる。DaVinci の Cloud Run / Firestore にある Issue 投影・Dashboard・業務 command の契約は別途維持・検討し、Reach の採用だけで即時廃止しない。

## 6. 検収

- DaVinci がローカル Reach から Codex app-server 等の実プロセスを起動し、同一 Session の複数 turn を処理できる。
- 入力の重複・順序違い・writer 競合を検出し、同じ Agent へ二重の入力が流れない。
- stdout / stderr の内容、offset、エラー、プロセス終了を再接続後も正しく取得できる。
- DaVinci Run → Reach Job / Process Session の参照が保存・復旧される。
- DaVinci の Controller を再起動しても、停止前の Job / 受理・配送不明を確定した事実に従って照合する。
- Reach Daemon 側の再起動を挟んだプロセス生存は、独立 OS supervisor による実測で確認する。確認できない場合は `unknown` として再起動しない。
- DaVinci の既存 Issue、要求変更、質問への回答、Sub Issue、検証、draft PR、停止・復旧の契約が維持される。
- Reach 側の Sandbox がプロセス・Workspace・秘密情報を隔離し、許可外の操作が拒否される。
- 任意の MCP クライアントが DaVinci の稼働なしで Reach を使用できる。
- Remote Cloudflare 接続不良でも、受理済みの Linux Job / Session と DaVinci の業務状態を区別して復旧できる。

## 7. 詳細設計で残す判断

- DaVinci の実 Agent protocol ごとの byte stream、PTY / pipe、backpressure、subprocess FD の必要条件
- Reach の Process Session に対する単一 writer lease / generation / recovery の詳細
- Process と Daemon が独立して生存する systemd unit / cgroup / sandbox の具体構成
- ローカル接続の認証・権限・credentials injection と取消し規約
- DaVinci の workspace clone・cleanup・Git 実行をどこまで Reach Capability に移すか
- DaVinci の保存済み Run に Reach ID / session / input receipt をどう持たせるか
- 現行 DaVinci と Reach の切替・rollback 時に所有者を二重化しない方法

関連資料：[Reach Product](../PRODUCT.md) / [Reach Architecture](../ARCHITECTURE.md) / [DaVinci Repository](https://github.com/takezoh/davinci)
