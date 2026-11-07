# Syntoria Reach — Technical Architecture

- Status: **Technical overview / Stages 1–2 implemented**
- Updated: 2026-11-01
- Initial runtime: **Linux / Go**
- Adopted remote access: **Cloudflare Workers + Durable Objects + outbound Linux WebSocket (Option B)**

## 1. Goals and structural principles

Reach is an independent execution service through which remote AIs and other local services use Linux capabilities via standard MCP or a local API. It places no always-on VM or container in the cloud; the Linux side owns the execution and state of operations, jobs, and agent sessions.

**DaVinci is the first real client service.** DaVinci's development decisions, prompts, judgment of agent output, and GitHub workflow stay in DaVinci; Linux processes, jobs, and agent sessions over Codex App Server / Claude Code stream-json / ACP are consolidated in Reach. Reach works without DaVinci.

**Architectural invariants**

1. Reach starts and works without depending on Weave, Lore, Trace, DaVinci, or a local LLM. Workspaces are provided by the Orca server on the same host (§7); callers reach them only through Reach.
2. MCP is an operation contract, separated from the communication path and from the Linux execution implementation.
3. The cloud handles connectivity, authentication, and forwarding; the source of truth for jobs is on Linux.
4. Even on an approved connection, the Reach runtime authorizes every operation.
5. Resending a request never causes a side effect to execute twice.
6. A communication failure or timeout is never treated as proof that execution failed.
7. In the MVP, requests made while a node is offline are not stored in a pending-execution queue.
8. What is installed on the target Linux machine is a lightweight connector / runtime; an LLM is not required.
9. Process liveness, jobs, and agent sessions (launching the harness and speaking its protocol: Codex App Server / Claude Code stream-json / ACP) are Reach's responsibility; sandboxing an agent's tools is the harness's responsibility. Reach does not take on the meaning of DaVinci runs or GitHub issues, and does not judge what an agent produces.
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
        PROC["Agent Session Manager: Codex / Claude / ACP"]
        JOB["Job Manager + OS process supervisor"]
        ST[("SQLite: Operations / Workspaces / Jobs / Sessions / Audit")]
        ORCA["Orca server: workspaces (Git worktrees)"]
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
| Agent Session Manager | Launching agent harnesses, Codex App Server / Claude Code stream-json / ACP sessions, one turn at a time, normalized events, interrupt and stop | Prompts, judging agent output, run semantics; sandboxing the agent's tools (the harness does that) |
| Operation Service | Re-validating arguments and permissions, idempotency decisions, accepting synchronous and asynchronous work | Business state of other Syntoria services |
| Capability Registry | Public contracts of File / Command / Job, required permissions, choice of OS adapter | External AI decisions |
| Linux Runtime | File operations in workspaces, process start/stop/isolation | MCP protocol details |
| Orca adapter | Creating and deleting workspaces (Orca worktrees) through the Orca CLI | Agents and commands in those workspaces (Reach runs them) |
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

Reach speaks Codex App Server, Claude Code stream-json, and ACP to the harness. DaVinci builds prompts and interprets the agent's output; Reach records turns and events as facts and does not infer the success of an LLM turn or the validity of development results.

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
| list_capabilities | Query a node's capabilities | node_id, capabilities |
| list_projects | Projects the caller may create workspaces from | project_id, permissions |
| create_workspace | Create an Orca workspace (worktree) from a project | operation_id, project_id, name → workspace_id, path |
| list_workspaces | The caller's workspaces | workspace_id, project_id, path |
| delete_workspace | Delete one of the caller's workspaces through Orca | operation_id, workspace_id |
| list_files | File listing within a permitted area | workspace_id, relative_path |
| read_file | Read a file | workspace_id, relative_path, bounded data |
| write_file | Conditional file write | operation_id, workspace_id, path, expected_version |
| start_job | Start an asynchronous command | operation_id, workspace_id, executable, argv, limits |
| start_agent_session | Start an agent harness and open a protocol session | operation_id, workspace_id, agent, resume_session_id |
| send_agent_prompt | Start one turn without waiting for it | operation_id, session_id, text → turn |
| read_agent_events | Read normalized events by sequence | session_id, after, limit |
| get_agent_session | Query a session | session_id → state, turns, protocol_session_id |
| stop_agent_session | Interrupt any turn and end the harness | operation_id, session_id |
| get_operation | Reconcile an unknown acceptance result | operation_id, admission / final state |
| get_job | Current state of a job | job_id, status, timestamps |
| read_job_output | Paged output retrieval | job_id, stream, cursor, limit |
| cancel_job | Request a job stop | operation_id, job_id |

IDs of jobs and agent sessions are generated on Linux. Start, execution state, and output retrieval are separated so results can be looked up later. A future adapter can serve hosts that support MCP Tasks, but the internal job contract does not depend on it.

The agent session tools are also provided to the DaVinci local client, which uses the same execution API. How tools are exposed to AI hosts and how streaming is supported are concerns of the connection adapter; the core defines the meaning of sessions, turns, and events.

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

Commands, jobs, and agent harnesses run as children of the daemon, each in its own process group, and are killed when the daemon dies; under the systemd user unit, the unit's cgroup also removes their descendants. **Work does not survive a daemon restart or a reboot.** Operations, jobs, and agent sessions left running by a previous daemon become unknown at the next start, and they are never re-executed automatically.

### Agent sessions (required for DaVinci's initial use)

- One agent session corresponds to one harness process owned by Reach and one protocol session (Codex thread / ACP session). Its ID is separate from DaVinci's run.
- Supported protocols, ported from DaVinci's agent adapters: Codex App Server and ACP v1 (newline-delimited JSON-RPC over stdio), and Claude Code's stream-json interface. For Claude, Reach appends the stream-json flags and `--session-id` / `--resume`. Agents (command, environment) are configured per node and allowed per project.
- A session runs one turn at a time. A prompt is an operation: resending it never starts a second turn, and a prompt while a turn is running is rejected.
- Agent messages, errors, and turn completion are recorded as events with a monotonic sequence and read back by cursor, so a transport disconnect loses no output.
- Disconnects of WSS / MCP / DaVinci are detach; only `stop_agent_session` interrupts the turn and ends the harness.
- Sandboxing belongs to the harness. Codex runs with its sandbox mode (default `workspace-write`) and approvals disabled. Claude Code runs with the permission mode and sandbox settings of its configured command and settings. ACP agents get no file system or terminal capabilities from Reach, and their permission requests are denied unless the agent's configuration allows them.
- After a daemon restart, sessions are unknown. A caller can start a new session that continues the conversation of one of its own ended sessions (`resume_session_id`, same workspace and agent); Reach never resumes on its own. Protocol session IDs are never accepted from callers, so no client can attach to another client's conversation.

## 7. Linux execution

Initial target: Linux with systemd, single user, single node.

- Daemon: a non-root systemd user service. Linger is set explicitly when needed.
- Storage: local SQLite (WAL) and managed log / artifact directories.
- Workspaces: a caller creates a workspace from a project (a repository registered in the Orca server of the same host). Reach creates it as an Orca worktree with its own branch through the Orca CLI and records it; only that caller can use and delete it. Deletion goes through Orca, deletes the branch only if Orca can prove it is merged, and is refused while jobs or agent sessions run or when the worktree has uncommitted changes. Workspaces that Reach did not create are never deleted.
- Files: take a workspace ID and a relative path, and enforce the boundary with safe path resolution such as openat2. Escapes via symlinks or mounts are also checked.
- Writes: conditional updates, with temporary files / atomic rename as needed.
- Commands and jobs: an executable from the project's allowlist plus argv; no implicit shell expansion; a minimal environment (the daemon's environment is not inherited); execution time and output size limits.
- Agents: sandboxing of the agent's tools is delegated to the agent harness (see §6).

**Note:** Reach does not isolate commands and jobs itself. Only allowlisted executables run, as the daemon's user, so allowlists should not include executables that can run arbitrary code unless that is intended.

## 8. Authentication, authorization, audit

### Cloud MCP

Verify external clients' OAuth/OIDC authentication and the binding between Reach users, clients, and nodes. Syntoria Gate can be used as the issuer, but it is not a required runtime dependency of the Reach core. Token issuer / audience / expiry / scopes etc. are verified, and users and nodes are never determined from unverified request parameters.

### Linux runtime

For both the DaVinci local API and requests from the cloud, verify the caller's authenticated principal, the owner of the job / session, and the permitted workspaces / capabilities. Credentials and model tokens are never mixed into arbitrary remote MCP text or logs; when needed, they are passed through limited paths such as dedicated FDs / a credential broker on the host.

Requests from the cloud are also bound to the caller's verified identity and an authorization context the server can verify. The Linux side also performs final authorization for node, workspace, capability, and the operation and its arguments. External documents and LLM-generated strings are never authentication or permission.

### Audit

Record the operation ID, authenticated principal, node, workspace, operation type, timestamp, result, and denial reason. Sensitive data, API keys, file contents, and environment variables are not logged unconditionally. Log retention period and maximum size are configurable.

## 9. Stored data and ownership boundaries

| Store | Contents | Authority |
| --- | --- | --- |
| Linux SQLite | operations, workspaces, jobs, agent_sessions, agent_events, audit_records | **Source of truth for operations / workspace ownership / jobs / agent sessions** |
| Orca server | Worktrees, branches, and their files | **Source of truth for workspace contents** |
| Linux files | Job stdout / stderr, harness stderr logs | **Source of truth for the actual data** |
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
| Orca unavailable or refusing | create / delete fail with orca_failed; existing workspaces stay usable. A daemon crash during create / delete leaves the workspace unknown |

Error classes: unauthorized / permission_denied / invalid_request / not_found / operation_conflict / version_conflict / invalid_state / harness_unavailable / orca_failed / internal; the remote path adds node_offline. An outcome that cannot be confirmed is the `unknown` state with `result_unknown`.

The availability goal is not "the WebSocket never drops" but **"even when it drops, the truth about accepted operations is never lost, and reconnection reconciles it correctly."**

## 11. MVP build order

1. **Local Core**: Go daemon, SQLite, workspaces (Orca), File / Command API, operation IDs / audit.
2. **Linux Execution**: asynchronous jobs, **agent sessions over Codex App Server / Claude Code stream-json / ACP** (sandboxing delegated to the harness), stop and ownership, reconciliation after restart.
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
- DaVinci starts Codex / ACP agents through Reach, sends multiple turns to the same session, and re-reads the events in order.
- The meaning and state of DaVinci runs / agent turns / GitHub issues / draft PRs are not confused with Reach jobs.
- DaVinci Controller restarts, duplicate prompts, session stop, and provider credential separation are verified with real processes.
- The mapping between Reach job / agent session IDs and DaVinci run IDs is stored in DaVinci and can be reconciled after recovery.

## 12. Items requiring design verification

| Topic | What to verify before implementation |
| --- | --- |
| Remote MCP | Compatibility of target MCP hosts and the chosen SDK with 2026-07-28 / 2025-11-25 |
| Authentication | Connection registration, revocation, and permission mapping when using an OAuth issuer / Gate |
| Node credential | Issuance, secret storage, expiry, revocation, re-registration |
| Harness sandbox | What Codex sandbox modes, Claude Code permission and sandbox settings, and ACP agents' permission handling actually confine on the target Linux |
| Broker | WebSocket restoration after DO hibernation, concurrent requests, correlation on disconnect |
| MCP responses | Tool timeouts, job acceptance ACKs, reconciliation on unknown results |
| Data | Retention and deletion of operations, events, job output, and logs |
| Orca | Stability of the Orca CLI's JSON results across versions; which base ref new workspaces should start from |
| DaVinci integration | A migration plan that keeps the existing run / prompt / credential contracts on Reach agent sessions |
| Agent events | Which harness events (tool calls, usage, plans) to record beyond messages, errors, and turn completion |

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
