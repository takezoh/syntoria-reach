# Syntoria Reach — Technical Architecture

- Status: **Technical overview / Stages 1–2 implemented, Stage 4 in part (remote MCP for workspaces and agent sessions, one node)**
- Updated: 2026-11-08
- Initial runtime: **Linux / Go**
- Adopted remote access: **Cloudflare Workers + Durable Objects + outbound Linux WebSocket (Option B)**

## 1. Goals and structural principles

Reach is an independent execution service through which remote AIs and other local services use Linux capabilities via standard MCP or a local API. It places no always-on VM or container in the cloud; the Linux side owns the execution and state of operations, jobs, and agent sessions.

**DaVinci is the first real client service.** DaVinci's development decisions, prompts, judgment of agent output, and GitHub workflow stay in DaVinci; Linux processes, jobs, and agent sessions (agents Reach starts in Orca terminals) are consolidated in Reach. Reach works without DaVinci.

**Architectural invariants**

1. Reach starts and works without depending on Weave, Lore, Trace, DaVinci, or a local LLM. Workspaces are provided by the Orca server on the same host (§7); callers reach them only through Reach.
2. MCP is an operation contract, separated from the communication path and from the Linux execution implementation.
3. The cloud handles connectivity, authentication, and forwarding; the source of truth for jobs is on Linux.
4. Even on an approved connection, the Reach runtime authorizes every operation.
5. Resending a request never causes a side effect to execute twice.
6. A communication failure or timeout is never treated as proof that execution failed.
7. In the MVP, requests made while a node is offline are not stored in a pending-execution queue.
8. What is installed on the target Linux machine is a lightweight connector / runtime; an LLM is not required.
9. Process liveness and jobs are Reach's responsibility. Agents run on Orca: Reach starts each in an Orca terminal of a workspace and relays prompts and replies through Orca; sandboxing an agent's tools is the agent's own responsibility. Reach does not take on the meaning of DaVinci runs or GitHub issues, and does not judge what an agent produces.
10. When a client disconnects, the lifecycle of an already accepted command, job, or agent turn does not change implicitly.

## 2. Overall structure

~~~mermaid
flowchart TB
    Clients["ChatGPT / Claude / Codex / other MCP hosts"]
    DaVinci["DaVinci Controller"]
    subgraph CF["Cloudflare — serverless"]
        GW["Workers: Stateless MCP Gateway / Auth / Routing"]
        BR["Durable Objects: Per-node Connection Broker"]
        GW <--> BR
    end
    subgraph Host["Linux — user-managed"]
        REACH["Reach Daemon (Go): Connector + Operation Service"]
        LOCAL["Local API (Unix socket)"]
        CAP["Capability adapters: File / Command"]
        PROC["Agent sessions: agents in Orca terminals"]
        JOB["Job Manager + OS process supervisor"]
        ST[("SQLite: Operations / Jobs / Audit")]
        ORCA["Orca server: workspaces (projects reach-*)"]
        LOCAL <--> REACH
        REACH --> CAP
        REACH --> JOB
        REACH --> PROC
        REACH <--> ST
        REACH -->|"Orca CLI"| ORCA
        PROC --> JOB
        JOB --> CAP
    end
    Clients <-->|"MCP Streamable HTTP"| GW
    BR <-->|"Outbound WSS established by Linux"| REACH
    DaVinci <-->|"Local execution API"| LOCAL
~~~

The standard deployment on Linux is a single Go daemon. The cloud has no resident containers, but unattended remote access requires the Linux connector itself to stay resident.

## 3. Components and responsibilities

| Component | Responsibility | Does not hold |
| --- | --- | --- |
| Workers MCP Gateway | MCP requests, client authentication/authorization, node resolution, input validation, broker calls | Source of truth for Linux jobs |
| Durable Object / Node Broker | Accepting and mapping the WSS connection for a node, message delivery, short-lived response correlation | User files, persistent job results |
| Reach Connector (Go) | Outbound WSS to the cloud, receiving authenticated requests, reconnection, transport separation | LLM inference |
| Local API (Unix socket) | Provides local services such as DaVinci with the same execution operations as the core | Meaning of issues/PRs |
| Agent sessions | Starting an agent in an Orca terminal of a workspace, typing prompts into it, reading its transcript through Orca, closing it | Prompts, judging agent output, run semantics; running the agent and sandboxing its tools (Orca and the agent do that) |
| Operation Service | Re-validating arguments and permissions, idempotency decisions, accepting synchronous and asynchronous work | Business state of other Syntoria services |
| Capability Registry | Public contracts of File / Command / Job, required permissions, choice of OS adapter | External AI decisions |
| Linux Runtime | File operations in workspaces, process start/stop/isolation | MCP protocol details |
| Orca adapter | Registering, listing, annotating (comment, workspace status) and removing the Orca projects that are workspaces, through the Orca CLI | Agents and commands in those workspaces (Reach runs them) |
| Job Manager | Job state, logs and artifacts, stop, reconciliation after restart | Dependence on MCP connection liveness |
| SQLite | Local source of truth for operations, jobs, policy, and audit | Data of other Syntoria services |

By design, the MCP Gateway and the Transport Broker are separate components, but they can be operated together as Worker / Durable Object bindings in the same Cloudflare account.

### Notes on Durable Objects / Hibernation

A Durable Object maps to a stable ID that identifies the node. A WebSocket can be kept by Cloudflare during hibernation, but the object's memory is lost. On wake-up, connection identifiers are reconstructed from WebSocket attachments and the like. **In-memory maps and pending Promises are never treated as persistent state.**

WebSocket disconnects and Cloudflare-side restarts are normal failure cases; the Linux side reconnects and re-registers. If the target node is absent, the gateway returns an explicit offline response.

## 4. Local and remote communication

### 4.0 Local connection for DaVinci

A DaVinci Controller on the same Linux node calls Reach's local API over a Unix domain socket. The request DTOs and the operation / job / agent session contracts are shared with the remote MCP adapter, and the call does not go through Cloudflare.

Local connections also verify the caller's identity and permissions. Unix socket OS permissions are not used as a substitute for global operation permissions; a permission profile for DaVinci limits the workspaces, commands, and agents it may use.

Reach relays prompts to an agent in an Orca terminal and reads its transcript back through Orca. The caller builds prompts and interprets the agent's output; Reach does not infer the success of an LLM turn or the validity of development results.

## 4.1 Remote communication and authentication

### 4.2 Node registration and persistent connection

1. The Linux user registers the node and explicitly issues a connection credential.
2. The daemon connects to Cloudflare over outbound WSS (no publicly exposed inbound port is needed).
3. The gateway / broker verifies the credential, the node ID, and the connection's validity.
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
| list_nodes | Query authorized nodes | node_id, online / offline |
| list_capabilities | Query a node's capabilities: the caller's permissions, commands and agents | node_id, permissions, commands, agents |
| create_workspace | Create a workspace for one unit of work: a folder registered as an Orca project, with a note | operation_id, name, note → workspace_id, name, note, status, path |
| list_workspaces | Every workspace, to find ongoing work | workspace_id, name, note, status, path |
| update_workspace | Change a workspace's note or Orca workspace status | operation_id, workspace_id, note, status |
| list_files | File listing within a permitted area | workspace_id, relative_path |
| read_file | Read a file | workspace_id, relative_path, bounded data |
| write_file | Conditional file write | operation_id, workspace_id, path, expected_version |
| start_job | Start an asynchronous command | operation_id, workspace_id, executable, argv, limits |
| start_agent_session | Start an agent in an Orca terminal of a workspace | operation_id, workspace_id, agent → session_id, terminal |
| send_agent_prompt | Type a prompt into the agent's terminal; returns once the turn started | operation_id, session_id, text → stages |
| read_agent_messages | Read the agent's transcript by cursor, as Orca reads it | session_id, cursor, limit → messages, cursor, activity |
| get_agent_session | Query a session as Orca reports it | session_id → workspace, terminal, liveness |
| stop_agent_session | Close the agent's terminal | operation_id, session_id |
| get_operation | Reconcile an unknown acceptance result | operation_id, admission / final state |
| get_job | Current state of a job | job_id, status, timestamps |
| read_job_output | Paged output retrieval | job_id, stream, cursor, limit |
| cancel_job | Request a job stop | operation_id, job_id |

IDs of jobs and agent sessions are generated on Linux. Start, execution state, and output retrieval are separated so results can be looked up later. A future adapter can serve hosts that support MCP Tasks, but the internal job contract does not depend on it.

The agent session tools are also provided to the DaVinci local client, which uses the same execution API. How tools are exposed to AI hosts and how streaming is supported are concerns of the connection adapter; the core defines the meaning of sessions, turns, and events.

**Implemented so far.** The remote MCP endpoint exposes the workspace and agent session tools and `get_operation`, with the names, arguments and results of the local API methods; the daemon serves both from one method table. The endpoint is stateless MCP Streamable HTTP with JSON responses, and Reach errors are tool errors carrying `{"error": {"code", "message"}}`. `list_nodes` and `list_capabilities` wait for multiple nodes; file, command and job tools for a later step.

Between the broker and the daemon, the WSS carries JSON text messages: the broker sends `{"id", "method", "params"}` and the daemon answers `{"id", "result"}` or `{"id", "error"}`. The ID is a relay correlation ID (§4.4). The daemon sends `ping` every 30 s, and the broker answers `pong` without waking from hibernation. A connection with no message for 90 s is replaced, and a new connection from the node replaces the old one. The broker waits 30 s for a reply. A request still in flight when the connection drops, or when the wait runs out, is `result_unknown`; with no connection at all, it is `node_offline`.

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

- An agent session is an agent running in an Orca terminal of a workspace. `start_agent_session` types the agent's configured command (shell-quoted) into a new terminal of the workspace's root entry (`orca terminal create --command`), and adopts the terminal as the Dispatch of a new Orca orchestration Run and Task (`orca orchestration dispatch`, without injecting anything), through which Orca reads the agent's transcript. The session ID is the Dispatch ID. Reach keeps nothing about sessions; they are read from Orca on every request and outlive the daemon.
- A prompt is an operation: Reach types it into the terminal (`orca terminal send --enter`), and the prompt is delivered once Orca observed the agent start its turn (`turn_started`); otherwise it fails with `harness_unavailable`, as when the agent waits for an answer in its terminal. Resending an operation types nothing again. A prompt sent during a turn is queued by the agent.
- A prompt is only a prompt. Orca bracket-pastes a prompt to a recognized agent and makes Escape inert, but writes it as typed to a terminal whose agent has quit, so Reach refuses a prompt when the terminal runs no agent. Reach also refuses text that the agent itself would run: Claude Code runs a prompt starting with `!` in the shell and one starting with `/` as one of its commands (checked with Claude Code 2.1.319), which would give `agent.run` what `command.run` and the agent's settings guard. A prompt has no control characters other than newlines and tabs and is at most 64 KiB, below Linux's 128 KiB limit on one command-line argument. While Orca reports the agent waiting for its user (`waitingOnHuman`, such as a permission prompt), Reach sends nothing: the answer is the user's, in Orca.
- Replies are read from the transcript (`orca orchestration worker-read --source transcript`): messages in Orca's form, a cursor to read on from, and the agent's activity (`done` while it waits for a prompt). The transcript is readable once the agent reported its session to Orca, in practice after the first prompt.
- `stop_agent_session` stops the Dispatch and closes the terminal, which ends the agent. Orca forgets the session the agent reported when its terminal closes, so the transcript and the workspace of the Dispatch are no longer known, and the session is `not_found` from then on, however the terminal closed. Disconnects of WSS / MCP / DaVinci are detach.
- Reach drives only sessions whose terminal (or, once closed, transcript) belongs to a Reach workspace, so a caller cannot reach other agents in Orca. Sessions are not owned by the client that started them.
- The agent runs in the user's login shell in the terminal, with its own settings and sandbox. Claude Code must start without a question to answer: the workspaces folder is trusted in `~/.claude.json` (trust applies to the folders inside) and the auto mode setup prompt is dismissed.
- Orca expects a Dispatch's agent to report `worker_done`; an agent in conversation does not, so Orca leaves a "stopped without reporting" message in the Run's mailbox after each turn. Reach does not read those mailboxes.

## 7. Linux execution

Initial target: Linux with systemd, single user, single node.

- Daemon: a non-root systemd user service. Linger is set explicitly when needed.
- Storage: local SQLite (WAL) and managed log / artifact directories.
- Workspaces: one workspace per unit of work. A caller creates one with a name and a note saying what the work is; Reach creates the folder `reach-<name>` under its `workspaces` folder and registers it in Orca as a folder project, with the note as the comment of the project's root entry. The workspace starts empty; the work brings in what it needs. Orca is the only record of a workspace: its existence, name, path, note and workspace status are read from Orca on every request, and Reach stores nothing about it, so a project changed or removed in Orca is changed or gone for Reach too. Reach sees only Orca projects whose name starts with `reach-`, and a client's permissions apply to every workspace. Callers do not delete workspaces: they set the status to `completed`, and the daemon, every hour, removes each completed workspace without activity for 7 days and nothing running — activity being Orca's last activity or Reach's latest audit record, agent session or job change for it, and nothing running meaning no active job or agent session and no request using the workspace (a request holds it from authorization until it returns, and cannot take one being removed). It removes the Orca project (with `--force`, which also detaches terminals open in Orca) and the folder, but only a folder it created under `workspaces`. Starting an agent in a completed workspace sets it back to `in-progress`.
- Files: take a workspace ID and a relative path, and enforce the boundary with safe path resolution such as openat2. Escapes via symlinks or mounts are also checked.
- Writes: conditional updates, with temporary files / atomic rename as needed.
- Commands and jobs: an executable from the configured allowlist plus argv; no implicit shell expansion; a minimal environment (the daemon's environment is not inherited); execution time and output size limits.
- Agents: run in Orca terminals in the user's login shell; sandboxing of the agent's tools is the agent's own (see §6).

**Note:** Reach does not isolate commands and jobs itself. Only allowlisted executables run, as the daemon's user, so allowlists should not include executables that can run arbitrary code unless that is intended.

## 8. Authentication, authorization, audit

### Cloud MCP

Verify external clients' OAuth/OIDC authentication and the binding between Reach users, clients, and nodes. Syntoria Gate can be used as the issuer, but it is not a required runtime dependency of the Reach core. Token issuer / audience / expiry / scopes etc. are verified, and users and nodes are never determined from unverified request parameters.

**Implemented so far (one user, one node).** The gateway is a resource server of Syntoria Gate and holds no OAuth state.
- **Clients and approval**: MCP hosts register with Gate dynamically and get Gate's opaque access tokens after the person signs in with GitHub and approves the connection. The gateway's protected-resource metadata names Gate as the authorization server.
- **Tokens**: every MCP request's token is introspected at Gate with the gateway's resource client credentials, and nothing is cached. The issuer, the audience (the gateway's MCP URL, a resource registered in Gate under its cookie parent), the expiry, Gate's approval and the client are checked, using the bearer verification of Gate's SDK. An invalid token is 401; Gate being unreachable is 503, never a sign-out.
- **Admission**: only the configured subject (`OWNER_SUBJECT`, `github:<ID>`) is admitted. Any other is 403.
- **The node**: it authenticates with the node token, a Worker secret.
- **Authorization on Linux**: every remote request acts as the one Reach client named in the daemon's `remote` config, and the daemon authorizes it with that client's permissions. The gateway asserts no other identity.
- **Revocation**: revoking a connection on Gate's `/connections` stops it at its next request. Removing `remote` or rotating the node token cuts off all remote access.

### Linux runtime

For both the DaVinci local API and requests from the cloud, verify the caller's authenticated principal, the owner of the job / session, and the permitted workspaces / capabilities. Credentials and model tokens are never mixed into arbitrary remote MCP text or logs; when needed, they are passed through limited paths such as dedicated FDs / a credential broker on the host.

Requests from the cloud are also bound to the caller's verified identity and an authorization context the server can verify. The Linux side also performs final authorization for node, workspace, capability, and the operation and its arguments. External documents and LLM-generated strings are never authentication or permission.

### Audit

Record the operation ID, authenticated principal, node, workspace, operation type, timestamp, result, and denial reason. Sensitive data, API keys, file contents, and environment variables are not logged unconditionally. Log retention period and maximum size are configurable.

## 9. Stored data and ownership boundaries

| Store | Contents | Authority |
| --- | --- | --- |
| Linux SQLite | operations, jobs, audit_records | **Source of truth for operations / jobs** |
| Orca server | Projects (`reach-*` are workspaces), their folders' registration, comments (notes) and workspace statuses; agents' terminals, Dispatches and transcripts | **Source of truth for workspaces and agent sessions** |
| Linux files | Job stdout / stderr | **Source of truth for the actual data** |
| Durable Object | WebSocket connection identifiers, minimal node connection metadata | For communication control. Not a source of truth for jobs |
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
| Daemon crash | Children die with the daemon. Running operations, jobs, and agent sessions become unknown at the next start and are never resumed |
| DaVinci restart | Look up the Reach job / agent session IDs saved in the DaVinci run, and read events from the last sequence it saw |
| Prompt delivery unknown | Reconcile by the prompt's operation ID (get_operation). Never resend the same prompt under a new ID |
| Duplicate requests | Prevent double side effects with SQLite unique constraints and argument comparison |
| Permission revoked | Reject new requests. Running jobs follow an explicit stop policy |
| Orca unavailable or refusing | Workspace requests fail with orca_failed (an unreachable Orca is never an empty list); running jobs and sessions continue, and cleanup waits for the next run. A daemon crash during create leaves the operation unknown; the workspace is whatever Orca reports |

Error classes: unauthorized / permission_denied / invalid_request / not_found / operation_conflict / version_conflict / invalid_state / harness_unavailable / orca_failed / internal; the remote path adds node_offline. An outcome that cannot be confirmed is the `unknown` state with `result_unknown`.

The availability goal is not "the WebSocket never drops" but **"even when it drops, the truth about accepted operations is never lost, and reconnection reconciles it correctly."**

## 11. MVP build order

1. **Local Core**: Go daemon, SQLite, workspaces (Orca), File / Command API, operation IDs / audit.
2. **Linux Execution**: asynchronous jobs, **agent sessions in Orca terminals** (sandboxing is the agent's), stop, reconciliation after restart.
3. **DaVinci local integration**: move DaVinci's agent execution onto Reach agent sessions while keeping its prompts and development workflow. Verify a local E2E.
4. **Remote MCP**: Workers MCP Gateway, OAuth, node registration, DO WebSocket broker, connector WSS.
5. **Acceptance / rollout**: verification on real Linux / hosted Cloudflare / multiple MCP hosts, plus staged cutover and recovery tests for the existing DaVinci.

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
- DaVinci starts agents through Reach, sends multiple turns to the same session, and re-reads the transcript by cursor.
- The meaning and state of DaVinci runs / agent turns / GitHub issues / draft PRs are not confused with Reach jobs.
- DaVinci Controller restarts, duplicate prompts, session stop, and provider credential separation are verified with real processes.
- The mapping between Reach job / agent session IDs and DaVinci run IDs is stored in DaVinci and can be reconciled after recovery.

## 12. Items requiring design verification

| Topic | What to verify before implementation |
| --- | --- |
| Remote MCP | Compatibility of target MCP hosts and the chosen SDK with 2026-07-28 / 2025-11-25 |
| Authentication | Connection registration, revocation, and permission mapping when using an OAuth issuer / Gate |
| Node credential | Issuance, secret storage, expiry, revocation, re-registration |
| Agent sandbox | What Claude Code's permission and sandbox settings actually confine for an agent in an Orca terminal on the target Linux |
| Broker | WebSocket restoration after DO hibernation, concurrent requests, correlation on disconnect |
| MCP responses | Tool timeouts, job acceptance ACKs, reconciliation on unknown results |
| Data | Retention and deletion of operations, job output, and logs; Orca's Runs and Dispatches left by agent sessions |
| Orca | Stability of the Orca CLI's JSON results across versions; which base ref new workspaces should start from |
| DaVinci integration | A migration plan that keeps the existing run / prompt / credential contracts on Reach agent sessions |
| Agents on Orca | Agents other than Claude Code in Orca terminals; questions an agent asks in its terminal (Orca's `waitingOnHuman`) |

These are decided in detailed design and verification on real environments. Design documents do not indicate running functionality or completed production acceptance.

## 13. DaVinci execution environment integration

As Reach's first client service, DaVinci uses the local API to delegate execution of agent processes, builds, tests, and verification. DaVinci's issues/PRs, prompts and output contracts, runs / attempts, and judgment and acceptance of results are not moved into Reach.

DaVinci's existing Go implementation (process / agent adapters / command delivery / recovery) serves as the reference implementation for design and tests and is generalized into Reach one responsibility at a time. Because the existing code has DaVinci-specific dependencies and termination conditions, wholesale copying or stopping the old controller first is not done.

DaVinci's current Cloud Run / Firestore issue projection, dashboard, and command delivery are business contracts of a separate product. What Reach's Cloudflare broker replaces is only the generic execution connection; DaVinci's business cloud features are not replaced unconditionally.

For the implementation and staged migration boundaries and acceptance items, see [DaVinci Execution Integration](docs/davinci-execution-integration.md).

## 14. Reference specifications

- [Cloudflare: Stateless MCP Handler](https://developers.cloudflare.com/agents/model-context-protocol/apis/handler-api/)
- [Cloudflare: Durable Objects WebSocket Hibernation](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [MCP specification](https://modelcontextprotocol.io/specification/)

Related: [README](README.md) / [Product Definition](PRODUCT.md) / [DaVinci Execution Integration](docs/davinci-execution-integration.md)
