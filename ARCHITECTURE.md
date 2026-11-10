# Syntoria Reach — Technical Architecture

- Status: **Technical overview / Stages 1–2 implemented, Stage 4 in part (remote MCP for workspaces and agent sessions on multiple nodes)**
- Updated: 2026-11-08
- Initial runtime: **Linux / Go**
- Adopted remote access: **Cloudflare Workers + Durable Objects + outbound Linux WebSocket (Option B)**

## 1. Goals and structural principles

Reach is an independent execution service through which remote AIs and other local services use Linux capabilities via standard MCP or a local API. It places no always-on VM or container in the cloud; the Linux side owns the execution and state of operations, jobs, and agent sessions.

**Reach is an execution environment and an interface to agents.** Programs that drive agents, **Controllers** (§13), run on top of it: their prompts, judgment of agent output and workflows stay with them, while Linux processes, jobs, workspaces and agent sessions (agents Reach starts in Orca terminals, or as its own processes) are Reach's. Reach works without any Controller.

**Architectural invariants**

1. Reach starts and works without depending on Weave, Lore, Trace, any Controller, or a local LLM. Workspaces are provided by the node's workspace backend: the Orca server on the same host, or Reach's own local workspaces (§7); callers reach them only through Reach.
2. MCP is an operation contract, separated from the communication path and from the Linux execution implementation.
3. The cloud handles connectivity, authentication, and forwarding; the source of truth for jobs is on Linux.
4. Even on an approved connection, the Reach runtime authorizes every operation.
5. Resending a request never causes a side effect to execute twice.
6. A communication failure or timeout is never treated as proof that execution failed.
7. In the MVP, requests made while a node is offline are not stored in a pending-execution queue.
8. What is installed on the target Linux machine is a lightweight connector / runtime; an LLM is not required.
9. Process liveness and jobs are Reach's responsibility. On Orca, Reach starts each agent in an Orca terminal of a workspace and relays prompts and replies through Orca; without Orca, it runs the agent as its own process and talks to it over the agent's protocol. Sandboxing an agent's tools is the agent's own responsibility. Reach does not take on the meaning of a caller's work, and does not judge what an agent produces.
10. When a client disconnects, the lifecycle of an already accepted command, job, or agent turn does not change implicitly.

## 2. Overall structure

~~~mermaid
flowchart TB
    Clients["ChatGPT / Claude / Codex / other MCP hosts"]
    Ctl["Controllers (MCP over stdio or HTTP)"]
    subgraph CF["Cloudflare — serverless"]
        GW["Workers: Stateless MCP Gateway / Auth / Routing"]
        BR["Durable Objects: Per-node Connection Broker"]
        GW <--> BR
    end
    subgraph Host["Linux — user-managed"]
        REACH["Reach Daemon (Go): Connector + Operation Service"]
        LOCAL["Local API (Unix socket)"]
        CAP["Capability adapters: File / Command"]
        WS["Workspace backend: Orca (orcaws) or local (localws)"]
        PROC["Agent sessions: agents in Orca terminals, or agent processes of local workspaces"]
        JOB["Job Manager + OS process supervisor"]
        ST[("SQLite: Operations / Jobs / Audit")]
        ORCA["Orca server: workspaces (projects reach-*)"]
        LOCAL <--> REACH
        REACH --> CAP
        REACH --> JOB
        REACH --> WS
        WS --> PROC
        REACH <--> ST
        WS -->|"Orca CLI"| ORCA
        PROC --> JOB
        JOB --> CAP
    end
    Clients <-->|"MCP Streamable HTTP"| GW
    BR <-->|"Outbound WSS established by Linux"| REACH
    REACH <-->|"MCP"| Ctl
    Ctl <-->|"Local execution API"| LOCAL
~~~

The standard deployment on each Linux node is a single Go daemon; one gateway serves all of a person's nodes, each with its own broker. The cloud has no resident containers, but unattended remote access requires the Linux connector itself to stay resident.

## 3. Components and responsibilities

| Component | Responsibility | Does not hold |
| --- | --- | --- |
| Workers MCP Gateway | MCP requests, client authentication/authorization, the node registry (`NODES`) and the list of nodes with their agents, node resolution, input validation, broker calls | Source of truth for Linux jobs |
| Durable Object / Node Broker | One per node: accepting the WSS connection of that node, message delivery, short-lived response correlation | User files, persistent job results |
| Reach Connector (Go) | Outbound WSS to the cloud, receiving authenticated requests, reconnection, transport separation | LLM inference |
| Local API (Unix socket) | Provides local services such as Controllers with the same execution operations as the core | Meaning of a caller's work |
| Controller connections | Registered Controllers: one MCP connection per Controller (starting and stopping stdio children, connecting to resident HTTP ones) and forwarding remote calls to their tools within the remote client's permissions (§13) | What a Controller's tools mean; receipts for Controller calls |
| Workspace backend | The service's Workspaces port: creating, listing, annotating and removing workspaces, and starting, prompting, reading and stopping their agents. `orcaws` does it on the Orca server, `localws` with folders, records in SQLite and agent processes | Authorization, idempotency, audit and cleanup decisions (the Operation Service's) |
| Agent sessions | On Orca: starting an agent in an Orca terminal of a workspace, typing prompts into it, reading its replies through Orca, closing it. Locally: running the agent's process in the workspace folder over its protocol (Claude Code stream-json, Codex App Server, ACP), one turn at a time | Prompts, judging agent output, run semantics; sandboxing the agent's tools (the agent does that) |
| Operation Service | Re-validating arguments and permissions, idempotency decisions, accepting synchronous and asynchronous work | Business state of other Syntoria services |
| Capability Registry | Public contracts of File / Command / Job, required permissions, choice of OS adapter | External AI decisions |
| Linux Runtime | File operations in workspaces, process start/stop/isolation | MCP protocol details |
| Orca adapter | The Orca CLI: projects, their entries (comment, workspace status), terminals and agent status, as the Orca backend uses them | What a workspace or session is (the Orca backend decides) |
| Job Manager | Job state, logs and artifacts, stop, reconciliation after restart | Dependence on MCP connection liveness |
| SQLite | Local source of truth for operations, jobs, policy, audit, and the workspaces of the local backend | Data of other Syntoria services |

By design, the MCP Gateway and the Transport Broker are separate components, but they can be operated together as Worker / Durable Object bindings in the same Cloudflare account.

### Notes on Durable Objects / Hibernation

A Durable Object maps to a stable ID that identifies the node. A WebSocket can be kept by Cloudflare during hibernation, but the object's memory is lost. On wake-up, connection identifiers are reconstructed from WebSocket attachments and the like. **In-memory maps and pending Promises are never treated as persistent state.**

WebSocket disconnects and Cloudflare-side restarts are normal failure cases; the Linux side reconnects and re-registers. If the target node is absent, the gateway returns an explicit offline response.

## 4. Local and remote communication

### 4.0 Local connection

A local service on the same Linux node, such as a Controller, calls Reach's local API over a Unix domain socket. The request DTOs and the operation / job / agent session contracts are shared with the remote MCP adapter, and the call does not go through Cloudflare.

Local connections also verify the caller's identity and permissions. Unix socket OS permissions are not used as a substitute for global operation permissions; each client's permissions limit the workspaces, commands, and agents it may use.

Reach relays prompts to an agent (in an Orca terminal, or a process of its own) and reads its reply back. The caller builds prompts and interprets the agent's output; Reach does not infer the success of an LLM turn or the validity of development results.

## 4.1 Remote communication and authentication

### 4.2 Node registration and persistent connection

1. The Linux user registers the node and explicitly issues a connection credential: an entry of the gateway's `NODES` secret, the node's ID and a token of its own.
2. The daemon connects to Cloudflare over outbound WSS (no publicly exposed inbound port is needed), with its node's token.
3. The gateway finds the node whose token it is (every token compared in constant time) and hands the connection to that node's broker.
4. The broker manages the connection and marks the node offline when the connection drops.
5. The daemon reconnects with exponential backoff and re-registers the necessary state once the connection is restored.

The credential's storage location has restricted permissions, and expiry, revocation, and rotation are designed in. A WSS connection to the node alone never authorizes arbitrary clients to run commands.

### 4.3 Requests from AI

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

The gateway exposes a stable remote MCP URL. External clients are not required to handle WebSocket or WebRTC directly. The MCP protocol lifecycle and Reach's explicit job / operation IDs are separate things.

### 4.4 Communication boundaries

- MCP is used over HTTP. The Linux connector's WSS is an internal request/response forwarding path.
- Connection liveness checks are not evidence of a job's success, failure, or completion.
- Waiting on the cloud side is kept short; long-running work is returned as a Linux job.
- On remote failures, the gateway does not resend side-effecting operations under a different ID.
- Relay message IDs / correlation IDs are distinct from operation IDs.
- P2P / WebRTC, dedicated tunnels, and VPNs are future transport adapters. The MVP works through Cloudflare alone.

## 5. Public MCP contract (initial draft)

The initial MCP tools are the following candidates. Formal names and JSON Schemas are decided in the implementation design.

| Tool | Purpose | Main inputs / results |
| --- | --- | --- |
| list_nodes | Query the person's nodes, with the agents each one can run and their roles | → node_id, online / offline, agents (id, role) |
| list_capabilities | Query a node's capabilities: the caller's permissions, commands and agents | node_id, permissions, commands, agents (id, role) |
| create_workspace | Create a workspace for one unit of work: a folder with a note (on Orca, registered as a project) | operation_id, name, note → workspace_id, name, note, status, path |
| list_workspaces | Every workspace, to find ongoing work | workspace_id, name, note, status, path |
| update_workspace | Change a workspace's note or Orca workspace status | operation_id, workspace_id, note, status |
| list_files | File listing within a permitted area | workspace_id, relative_path |
| read_file | Read a file | workspace_id, relative_path, bounded data |
| write_file | Conditional file write | operation_id, workspace_id, path, expected_version |
| start_job | Start an asynchronous command | operation_id, workspace_id, executable, argv, limits |
| start_agent_session | Start an agent in a workspace (on Orca, in a terminal of it) | operation_id, workspace_id, agent → session_id, workspace, state |
| send_agent_prompt | Deliver a prompt to the agent (on Orca, type it into its terminal); returns once the turn started | operation_id, session_id, text → stages |
| read_agent_output | Read the agent's state and, once its turn ended, its reply | session_id → state, waiting_on_human, reply, reply_truncated |
| get_agent_session | Query a session as the node observes it | session_id → workspace, agent, state, waiting_on_human |
| stop_agent_session | End the agent and its session (on Orca, close its terminal) | operation_id, session_id |
| get_operation | Reconcile an unknown acceptance result | operation_id, admission / final state |
| get_job | Current state of a job | job_id, status, timestamps |
| read_job_output | Paged output retrieval | job_id, stream, cursor, limit |
| cancel_job | Request a job stop | operation_id, job_id |

IDs of jobs and agent sessions are generated on Linux. Start, execution state, and output retrieval are separated so results can be looked up later. A future adapter can serve hosts that support MCP Tasks, but the internal job contract does not depend on it.

The agent session tools are also provided to local clients such as Controllers, which use the same execution API. How tools are exposed to AI hosts and how streaming is supported are concerns of the connection adapter; the core defines the meaning of sessions, turns, and events.

**Implemented so far.** The remote MCP endpoint serves all of a person's nodes. `list_nodes` lists the nodes registered in `NODES`, by ID, with whether each is connected and, for a connected node, the agents the remote client may start there with their roles: the gateway asks every node's `list_capabilities` at once, and a node that is not connected is offline. Every other tool is a local API method of one node, with the method's name, arguments and results plus a required `node`, which the gateway removes before it forwards the call to that node's broker; an unknown node is `not_found`, and nothing runs. The tools are `list_capabilities`, the workspace and agent session tools and `get_operation`; the daemon serves them from the method table of the local API. Workspace, session and operation IDs are a node's; the gateway adds no node to them. The endpoint is stateless MCP Streamable HTTP with JSON responses, and Reach errors are tool errors carrying `{"error": {"code", "message"}}`. File, command and job tools wait for a later step.

Between a broker and its daemon, the WSS carries JSON text messages: the broker sends `{"id", "method", "params"}` and the daemon answers `{"id", "result"}` or `{"id", "error"}`. The ID is a relay correlation ID (§4.4). The daemon sends `ping` every 30 s, and the broker answers `pong` without waking from hibernation. A connection with no message for 90 s is replaced, and a new connection from the node replaces the old one. The broker waits 30 s for a reply. A request still in flight when the connection drops, or when the wait runs out, is `result_unknown`; with no connection at all, it is `node_offline`.

## 6. Operations / jobs / idempotency

**Operations and jobs are separate.** An operation is the acceptance of a request and the uniqueness of its side effect; a job is the state of an ongoing execution.

Side-effecting requests require an operation ID. On receipt, the authenticated principal, a hash of the normalized arguments, and the acceptance state are recorded in SQLite in a transaction before the operation is executed.

- A resend with the same principal, operation ID, and arguments returns the existing result.
- The same operation ID with different arguments returns a conflict.
- If a response is lost, reconcile with get_operation / get_job.
- An existing operation of unknown outcome is never re-executed under a new ID.
- Loss of gateway / DO memory does not change the source of truth on Linux.

### Job states (draft)

queued → running → succeeded / failed

running → cancelling → cancelled / failed / unknown

unknown is used when the result or the OS-side state cannot be confirmed. State transitions, reasons, and the last observation time are stored. Acceptance of a stop request is kept distinct from the fact that the process has stopped.

Commands and jobs run as children of the daemon, each in its own process group, and are killed when the daemon dies; under the systemd user unit, the unit's cgroup also removes their descendants. **Commands and jobs do not survive a daemon restart or a reboot.** Operations and jobs left running by a previous daemon become unknown at the next start, and they are never re-executed automatically. Agents run in Orca terminals and survive a daemon restart.

### Agent sessions

The service decides what a caller may do with a session, and the node's workspace backend what a session is: the service authorizes, makes prompts and stops operations, sends a prompt only while the session's state is `idle`, and refuses text that is not a prompt (below); the backend starts the agent, reports its state and reply, delivers the prompt and ends the session. On both backends the states are `idle`, `working`, `waiting_on_human` (Orca only) and `exited`, a reply is the agent's last message of the turn, and an ended session is `not_found`.

**On Orca.**

- An agent session is an agent running in an Orca terminal of a workspace. `start_agent_session` types the agent's configured command (shell-quoted) into a new terminal of the workspace's root entry (`orca terminal create --command`) and returns once Orca sees the agent idle at its prompt (`orca terminal wait --for tui-idle`), or after 15 s, so that it answers within the remote gateway's 30 s; if no agent runs in the terminal by then, it closes the terminal and fails with `harness_unavailable`. The session ID is the terminal's Orca handle, which a client chooses and which reaches the Orca CLI only as `--flag=value`, never as a flag of its own. Reach keeps nothing about sessions; they are read from Orca on every request (`orca terminal show`) and outlive the daemon.
- A session's state is what Orca observes of its agent: `exited` when Orca sees no agent in the terminal (a shell), `waiting_on_human` while Orca saw it wait for its user (`agentWait` of `terminal show`, from its agent status hooks, the prompt text or the title), cannot tell (no `agentWait`), or sees it stopped at such a prompt while waiting for it (`blockedReason`), `idle` when Orca sees it idle at its prompt within 3 s (`tui-idle`) and its agent status does not report it in a turn, and `working` otherwise. At a permission prompt, `tui-idle` does not become idle and `agentWait` names the hook (checked with Claude Code 2.1.319).
- A prompt is an operation: Reach types it into the terminal (`orca terminal send --enter`), and the prompt is delivered once Orca observed the agent start its turn (`turn_started`); otherwise it fails with `harness_unavailable`, as when the agent waits for an answer in its terminal. Resending an operation types nothing again.
- A prompt is only a prompt. Orca bracket-pastes a prompt to a recognized agent and makes Escape inert, but writes it as typed to a terminal whose agent has quit, so Reach refuses a prompt when the terminal runs no agent. Reach also refuses text that the agent itself would run: Claude Code runs a prompt starting with `!` in the shell and one starting with `/` as one of its commands (checked with Claude Code 2.1.319), which would give `agent.run` what `command.run` and the agent's settings guard. A prompt has no control characters other than newlines and tabs and is at most 64 KiB, below Linux's 128 KiB limit on one command-line argument. Reach sends a prompt only while the state is `idle`. While the agent waits for its user, or Orca cannot tell, the answer is the user's, in Orca; during a turn, the agent could ask its user something just before the prompt arrives, and the prompt would answer it.
- Replies are read from Orca's agent status (`orca worktree ps`), which Orca keeps from the agents' status hooks (`orca agent hooks status`). The agent of a terminal is the row whose `paneKey` is the terminal's `<tabId>:<leafId>` from `terminal show`. Orca empties the agent's last message (`lastAssistantMessage`) when a prompt is submitted and sets it when the turn ends, from Claude Code's Stop hook (`last_assistant_message`), so the reply read once the state is `idle` answers the last prompt; the terminal can look idle before the hook reports, so the state stays `working` while the status reports the agent in a turn. The agent's own state is `mainAgent.state`: the row's `state` stays `working` while work the agent left running in the background goes on. A message that is a tool's output is not a reply. Orca keeps the first 8,000 UTF-16 code units of a message, trimmed and with runs of blank lines folded, and drops a high surrogate left alone at the cut; Reach reports a reply that reached that length as `reply_truncated` (checked with Orca 1.4.252 and Claude Code 2.1.319). The terminal's output is not read: the agent keeps its usual view, which the user reads in Orca, and that view repaints lines, which a reader of the output (`orca terminal read`) gets back as fragments.
- `stop_agent_session` closes the terminal, which ends the agent. Orca no longer has the terminal's handle (`terminal_handle_stale`), so the session is `not_found` from then on, however the terminal closed. Disconnects of WSS, MCP or a local client are detach.
- Every terminal in a Reach workspace is a session, and no other terminal is, so a caller cannot reach agents elsewhere in Orca. Sessions are not owned by the client that started them.
- The agent runs in the user's login shell in the terminal, with its own settings and sandbox. Claude Code must start without a question to answer: the workspaces folder is trusted in `~/.claude.json` (trust applies to the folders inside) and the auto mode setup prompt is dismissed.
- Reach does not use Orca's orchestration (Runs, Tasks, Dispatches). A Run needs the terminal of its coordinator, which Reach does not have: without `--from`, Orca guesses it from the terminals it has and fails with `no_active_sender_terminal` unless exactly one fits, and the one it picks may be the user's. Orca then reports each turn the agent ends without `worker_done` to the coordinator by typing a pointer to it into the coordinator's terminal, which would also be the agent's own if it coordinated itself.

**In local workspaces.**

- An agent session is an agent process the daemon runs in the workspace folder, with the agent's `command` and, in its environment, the fixed `PATH`, `LANG` and `HOME` of commands plus the agent's `env`. Reach talks to it over its `protocol`, relaying prompts and replies without prompts or output contracts of its own: Claude Code over stream-json (Reach adds `--print`, the stream-json flags and `--session-id`, and checks that the session is the one it asked for), Codex App Server (`thread/start` with approvals off; its sandbox is Codex's own configuration) or the Agent Client Protocol (no file system or terminal offered; permission requests are cancelled). No one answers an agent there: what it may do is its settings'.
- `start_agent_session` returns once the agent opened its protocol session, or after 15 s with the session `working`; an agent that does not start or open is `harness_unavailable` and leaves no session. The session ID is Reach's (`ses_…`).
- The state is `exited` once the process ended, `working` while it opens or runs a turn, and `idle` otherwise. A prompt starts a turn and the operation succeeds once it started; the reply is the agent's last message of the turn (Claude's `result`, Codex's last `agentMessage`, ACP's last message chunks), or the failure that ended it, of which Reach keeps the first 64 KiB (`reply_truncated` beyond that).
- Sessions are kept in memory, in the daemon: the agents are its children (own process group, killed with it), and after a restart their sessions are `not_found`. `stop_agent_session` ends the turn and the protocol session, kills the process group and forgets the session. An open session, even one whose agent exited, keeps its workspace busy. The agent's stderr is kept, bounded, in `agents/<session>.stderr` next to the database until the session is stopped.

## 7. Linux execution

Initial target: Linux with systemd, single user, one or more nodes.

- Daemon: a non-root systemd user service. Linger is set explicitly when needed.
- Storage: local SQLite (WAL) and managed log / artifact directories.
- Workspaces: one workspace per unit of work. A caller creates one with a name and a note saying what the work is; Reach creates the folder `reach-<name>` under its `workspaces` folder. The node's configuration chooses the workspace backend that keeps it: Orca when it names the Orca CLI, local workspaces otherwise.
- Local workspaces: Reach records the workspace (ID `ws_…`, name, folder, note, status, last change) in SQLite, and the record is the workspace; statuses are todo, in-progress, in-review and completed. Cleanup is the same as on Orca below, with the record's last change as the activity outside Reach; removal forgets the record and removes the folder. A folder `reach-*` under `workspaces` that no record has, as one left by a daemon that stopped while creating it, is removed once nothing in it has changed for 7 days.
- Workspaces on Orca: Reach registers the folder in Orca as a folder project, with the note as the comment of the project's root entry. The workspace starts empty; the work brings in what it needs. Orca is the only record of a workspace: its existence, name, path, note and workspace status are read from Orca on every request, and Reach stores nothing about it, so a project changed or removed in Orca is changed or gone for Reach too. Reach sees only Orca projects whose name starts with `reach-`, and a client's permissions apply to every workspace. Callers do not delete workspaces: they set the status to `completed`, and the daemon, every hour, removes each completed workspace without activity for 7 days and nothing running — activity being Orca's last activity or Reach's latest audit record, agent session or job change for it, and nothing running meaning no active job or agent session and no request using the workspace (a request holds it from authorization until it returns, and cannot take one being removed). It removes the Orca project (with `--force`, which also detaches terminals open in Orca) and the folder, but only a folder it created under `workspaces`. A project removed in Orca, outside Reach, leaves its folder: the daemon also removes a folder `reach-*` under `workspaces` that no Orca project has, under any name, once nothing in it (no file, folder or symlink, which it does not follow) has changed for 7 days, so that a folder being created, or one Orca leaves out of its list for a while, stays. Starting an agent in a completed workspace sets it back to `in-progress`.
- Files: take a workspace ID and a relative path, and enforce the boundary with safe path resolution such as openat2. Escapes via symlinks or mounts are also checked.
- Writes: conditional updates, with temporary files / atomic rename as needed.
- Commands and jobs: an executable from the configured allowlist plus argv; no implicit shell expansion; a minimal environment (the daemon's environment is not inherited); execution time and output size limits.
- Agents: on Orca, in Orca terminals in the user's login shell; in local workspaces, as processes of the daemon. Sandboxing of the agent's tools is the agent's own (see §6).

**Note:** Reach does not isolate commands and jobs itself. Only allowlisted executables run, as the daemon's user, so allowlists should not include executables that can run arbitrary code unless that is intended.

## 8. Authentication, authorization, audit

### Cloud MCP

The remote entry point uses Cloudflare Access Managed OAuth. Access owns client registration, login, OAuth token exchange and policy evaluation; the Worker owns validation of the authenticated assertion and node routing. No MCP Server Portal is required.

**Implemented (one user, multiple nodes).**
- **Assertions**: every MCP request must carry `Cf-Access-Jwt-Assertion`. `cloud/src/access.ts` verifies RS256 signatures using the configured issuer's `/cdn-cgi/access/certs`, exact issuer, the MCP application's audience, required expiry and issued-at claims, and the configured owner's email. It ignores client identity and does not accept bearer tokens or browser cookies as a substitute at the origin.
- **Admission**: `ACCESS_TEAM_DOMAIN`, `ACCESS_AUD` and `ACCESS_OWNER_EMAIL` identify the issuer, MCP application and owner email. Another authenticated email or an assertion without a nonempty user subject is 403; missing or invalid credentials are 401. Missing configuration or unavailable signing keys are 503. Signing keys are cached with the verifier library's bounded lifetime; assertion outcomes are not cached.
- **Edge boundary**: configure Access on `/mcp` only and enable Managed OAuth. Access supplies discovery and authentication challenges before origin requests. The Worker has no Gate protected-resource metadata. Alternative Worker URLs remain disabled. Protecting the whole hostname with interactive OAuth would break node connectivity.
- **Nodes**: `NODES` registers node IDs and their existing tokens. `/connect` independently authenticates each daemon with its node token and binds it to its own broker. This channel does not use Access user login. Missing or malformed `NODES` fails closed.
- **Authorization on Linux**: every remote request acts as the receiving daemon's `remote.client`; Linux still enforces that client's permissions. Neither the node mapping nor permissions change in this authentication migration.
- **Revocation**: Access policies and token lifetimes replace Gate's immediate per-request connection checks. Issued credentials can remain usable until expiry. Removing or rotating node credentials remains a separate operation. No per-AI identity or connection revocation is implemented in Reach.
- **Sessions**: long-running work remains on Linux. Clients poll `read_agent_output`; MCP Events is not implemented by this change. Local workerd tests do not prove hosted Claude or ChatGPT OAuth acceptance.

### Linux runtime

For both local API clients and requests from the cloud, verify the caller's authenticated principal, the owner of the job / session, and the permitted workspaces / capabilities. Credentials and model tokens are never mixed into arbitrary remote MCP text or logs; when needed, they are passed through limited paths such as dedicated FDs / a credential broker on the host.

Requests from the cloud are also bound to the caller's verified identity and an authorization context the server can verify. The Linux side also performs final authorization for node, workspace, capability, and the operation and its arguments. External documents and LLM-generated strings are never authentication or permission.

### Audit

Record the operation ID, authenticated principal, node, workspace, operation type, timestamp, result, and denial reason. Sensitive data, API keys, file contents, and environment variables are not logged unconditionally. Log retention period and maximum size are configurable.

## 9. Stored data and ownership boundaries

| Store | Contents | Authority |
| --- | --- | --- |
| Linux SQLite | operations, jobs, audit_records; local_workspaces (local workspace backend) | **Source of truth for operations / jobs, and for local workspaces** |
| Orca server | Projects (`reach-*` are workspaces), their folders' registration, comments (notes) and workspace statuses; agents' terminals and their output | **Source of truth for workspaces and agent sessions on Orca** |
| Daemon memory | Agent sessions of local workspaces (their processes are the daemon's children) | **Source of truth for local agent sessions**; lost, with the agents, when the daemon stops |
| Linux files | Job stdout / stderr | **Source of truth for the actual data** |
| Durable Object | One per node: its WebSocket connection identifiers, minimal node connection metadata | For communication control. Not a source of truth for jobs |
| Worker secrets | `NODES`: the node IDs and their tokens; Access issuer, audience and owner email | Node registry and credentials |
| Workers | Transient processing and forwarding of authenticated requests | Holds no persistent job state |

If a requirement arises to copy data to the Cloudflare side, a separate contract for what is stored, its sensitivity, retention period, and consistency is designed.

## 10. Failure contracts

| Failure | Expected behavior |
| --- | --- |
| Linux node offline | Return node_offline. The MVP does not queue automatically |
| WebSocket disconnect | Connector reconnects with backoff. Accepted jobs continue on Linux |
| Temporary Cloudflare outage | New operations may be unavailable. Linux jobs and results are kept |
| DO hibernation / recreation | Reconstruct connection metadata and request connector re-registration if needed |
| HTTP response lost | Return outcome_unknown; query or resend the same operation ID |
| Daemon crash | Children die with the daemon. Running operations and jobs become unknown at the next start and are never resumed. Agents on Orca live on; agents of local workspaces die with the daemon, and their sessions are not_found |
| Controller restart | The Controller looks up the Reach workspace, job and agent session IDs it saved and reattaches. Agents on Orca live on; a session is lost only when Reach reports it not_found or exited |
| Controller call outcome unknown | result_unknown. Reach keeps no receipt of Controller calls; the caller checks the effect with the Controller's own tools before calling again |
| Prompt delivery unknown | Reconcile by the prompt's operation ID (get_operation). Never resend the same prompt under a new ID |
| Duplicate requests | Prevent double side effects with SQLite unique constraints and argument comparison |
| Permission revoked | Reject new requests. Running jobs follow an explicit stop policy |
| Orca unavailable or refusing | Workspace requests fail with orca_failed (an unreachable Orca is never an empty list); running jobs and sessions continue, and cleanup waits for the next run. A daemon crash during create leaves the operation unknown; the workspace is whatever Orca reports |

Error classes: unauthorized / permission_denied / invalid_request / not_found / operation_conflict / version_conflict / invalid_state / harness_unavailable / orca_failed / internal; the remote path adds node_offline. An outcome that cannot be confirmed is the `unknown` state with `result_unknown`.

The availability goal is not "the WebSocket never drops" but **"even when it drops, the truth about accepted operations is never lost, and reconnection reconciles it correctly."**

## 11. MVP build order

1. **Local Core**: Go daemon, SQLite, workspaces (Orca), File / Command API, operation IDs / audit.
2. **Linux Execution**: asynchronous jobs, **agent sessions in Orca terminals** (sandboxing is the agent's), stop, reconciliation after restart.
3. **Controllers**: register Controllers on a node, connect to them over MCP, and forward remote calls to their tools; a Controller drives agents through the local API. Verify a local E2E.
4. **Remote MCP**: Workers MCP Gateway, OAuth, node registration, DO WebSocket broker, connector WSS.
5. **Acceptance / rollout**: verification on real Linux / hosted Cloudflare / multiple MCP hosts, and recovery tests with a real Controller.

### Required acceptance tests

- Reach on Linux can be used standalone without Cloudflare.
- An authenticated MCP client can access permitted workspaces.
- Paths outside the workspace, symlink escapes, unpermitted commands, and attempts to obtain secrets are rejected.
- After a job is started, disconnecting MCP / WSS does not stop the processing or the records on Linux.
- Resending the same operation ID does not cause double execution.
- Communication timeouts are handled correctly as unknown, and execution results can be reconciled.
- Jobs can be queried after DO hibernation / recreation or connector reconnection.
- Work that was not accepted while the node was offline is not executed on its own after recovery.
- Production OAuth, node credential revocation, and permission changes take effect.
- A Controller starts agents through Reach, sends multiple turns to the same session, and reads the reply of each turn.
- The meaning and state of a Controller's work are not confused with Reach jobs.
- Controller restarts, duplicate prompts, session stop, and the separation of the Controller's credentials from its agents are verified with real processes.
- Remote calls to Controller tools stay within the remote client's permissions, and a reply lost after the call is result_unknown.

## 12. Items requiring design verification

| Topic | What to verify before implementation |
| --- | --- |
| Remote MCP | Compatibility of target MCP hosts and the chosen SDK with 2026-07-28 / 2025-11-25 |
| Authentication | Managed OAuth client acceptance, policy changes, token expiry and permission mapping |
| Node credential | Issuance, secret storage, expiry, revocation, re-registration |
| Agent sandbox | What Claude Code's permission and sandbox settings actually confine for an agent in an Orca terminal on the target Linux |
| Broker | WebSocket restoration after DO hibernation, concurrent requests, correlation on disconnect |
| MCP responses | Tool timeouts, job acceptance ACKs, reconciliation on unknown results |
| Data | Retention and deletion of operations, job output, and logs; Orca's Runs and Dispatches left by the agent sessions of earlier versions |
| Orca | Stability of the Orca CLI's JSON results across versions; which base ref new workspaces should start from |
| Controllers | Real Controllers over stdio and HTTP on a real node through the deployed gateway |
| Agents on Orca | Agents other than Claude Code in Orca terminals; questions an agent asks in its terminal (Orca's `agentWait`); agents that write their conversation only as a full-screen view |
| Local agents | The real Claude Code, Codex and DSH as processes of the daemon (verified so far against fake agents speaking their protocols); what their settings must allow when no one answers their questions |

These are decided in detailed design and verification on real environments. Design documents do not indicate running functionality or completed production acceptance.

## 13. Controllers

A Controller is a program that drives agents: the loop between Reach and the agents on a node. Reach starts or connects to it as an MCP server, lets remote AIs call its tools, and the Controller uses the node's workspaces, agent sessions, files, commands and jobs through the local API like any local client. Reach does not know what a Controller's tools do, or which agent protocols a Controller speaks.

- **Registration.** The node config's `controllers` registers each Controller with a unique `id`, a `role` that callers choose by (calls name the ID), exactly one transport, `command` (an absolute executable and its arguments, stdio) or `url` (a resident Streamable HTTP MCP endpoint), and `client`, the configured local API client it acts as. A missing or unknown client, or one without a token, is a config error before anything starts.
- **Connections.** The node keeps one MCP connection per Controller, shared by all requests; it never starts a Controller per request. A stdio Controller is a child of the daemon: it gets the fixed child environment plus `REACH_SOCKET` and `REACH_TOKEN` (its client's token), Reach performs MCP `initialize`, and the child ends when the daemon shuts down. An HTTP Controller is resident and configured by its operator; Reach connects to it, treats a successful `initialize` as ready, and never stops it.
- **Authority.** Remote calls act as `remote.client`, whose `allow` grants a whole Controller (`controller:<JSON-quoted id>`) or one tool (`controller-tool:<JSON-quoted id>:<JSON-quoted tool>`). `list_controllers` and `list_controller_tools` show only what is granted, and a call that is not granted is refused before the Controller is contacted. Arguments never choose a principal or an endpoint.
- **Results and failures.** A tool result keeps the Controller's MCP content and `isError`. An error the Controller returns is `controller_error`; a Controller that is not running or not initialized is `harness_unavailable`; a call whose reply was lost is `result_unknown`. Reach keeps no receipt of Controller calls and `get_operation` does not apply to them, so after `result_unknown` the caller checks the effect with the Controller's own tools before calling again. The Controller's token never appears in listings, results or errors.

## 14. Reference specifications

- [Cloudflare: Stateless MCP Handler](https://developers.cloudflare.com/agents/model-context-protocol/apis/handler-api/)
- [Cloudflare: Durable Objects WebSocket Hibernation](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [MCP specification](https://modelcontextprotocol.io/specification/)

Related: [README](README.md) / [Product Definition](PRODUCT.md)
