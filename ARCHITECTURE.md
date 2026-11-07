# Syntoria Reach — Technical Architecture

- Status: **Technical overview / pre-implementation**
- Updated: 2026-11-01
- Initial runtime: **Linux / Go**
- Adopted remote access: **Cloudflare Workers + Durable Objects + outbound Linux WebSocket (Option B)**

## 1. Goals and structural principles

Reach is an independent execution service through which remote AIs and other local services use Linux capabilities via standard MCP or a local API. It places no always-on VM or container in the cloud; the Linux side owns the execution and state of operations, jobs, and interactive process sessions.

**DaVinci is the first real client service.** DaVinci's development decisions, agent protocols, and GitHub workflow stay in DaVinci; Linux process / sandbox / streaming I/O / job execution are consolidated in Reach. Reach works without DaVinci.

**Architectural invariants**

1. Reach starts and works without depending on Weave, Lore, Trace, DaVinci, or a local LLM.
2. MCP is an operation contract, separated from the communication path and from the Linux execution implementation.
3. The cloud handles connectivity, authentication, and forwarding; the source of truth for jobs is on Linux.
4. Even on an approved connection, the Reach runtime authorizes every operation.
5. Resending a request never causes a side effect to execute twice.
6. A communication failure or timeout is never treated as proof that execution failed.
7. In the MVP, requests made while a node is offline are not stored in a pending-execution queue.
8. What is installed on the target Linux machine is a lightweight connector / runtime; an LLM is not required.
9. Process liveness, I/O, sandbox, and jobs are Reach's responsibility; Reach does not take on the meaning of DaVinci runs, agent turns, or GitHub issues.
10. When a client attached to an interactive process disconnects, the lifecycle of the already accepted execution does not change implicitly.

## 2. Overall structure

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

The standard deployment on Linux is a single Go daemon. The cloud has no resident containers, but unattended remote access requires the Linux connector itself to stay resident.

## 3. Components and responsibilities

| Component | Responsibility | Does not hold |
| --- | --- | --- |
| Workers MCP Gateway | MCP requests, client authentication/authorization, node resolution, input validation, broker calls | Source of truth for Linux jobs |
| Durable Object / Node Broker | Accepting and mapping the WSS connection for a node, message delivery, short-lived response correlation | User files, persistent job results |
| Reach Connector (Go) | Outbound WSS to the cloud, receiving authenticated requests, reconnection, transport separation | LLM inference |
| Local API (Unix socket) | Provides local services such as DaVinci with the same execution operations as the core | Meaning of issues/PRs |
| Process Session Manager | stdin / stdout / stderr of long-lived processes, I/O ordering, single writer, reconnection and stop reconciliation | Agent-specific conversation protocols |
| Operation Service | Re-validating arguments and permissions, idempotency decisions, accepting synchronous and asynchronous work | Business state of other Syntoria services |
| Capability Registry | Public contracts of File / Command / Job, required permissions, choice of OS adapter | External AI decisions |
| Linux Runtime | File operations in workspaces, process start/stop/isolation | MCP protocol details |
| Job Manager | Job state, logs and artifacts, stop, reconciliation after restart | Dependence on MCP connection liveness |
| SQLite | Local source of truth for operations, jobs, policy, and audit | Data of other Syntoria services |

By design, the MCP Gateway and the Transport Broker are separate components, but they can be operated together as Worker / Durable Object bindings in the same Cloudflare account.

### Notes on Durable Objects / Hibernation

A Durable Object maps to a stable ID that identifies the node. A WebSocket can be kept by Cloudflare during hibernation, but the object's memory is lost. On wake-up, connection identifiers are reconstructed from WebSocket attachments and the like. **In-memory maps and pending Promises are never treated as persistent state.**

WebSocket disconnects and Cloudflare-side restarts are normal failure cases; the Linux side reconnects and re-registers. If the target node is absent, the gateway returns an explicit offline response.

## 4. Local and remote communication

### 4.0 Local connection for DaVinci

A DaVinci Controller on the same Linux node calls Reach's local API over a Unix domain socket. The request DTOs and the operation / job / process session contracts are shared with the remote MCP adapter, and the call does not go through Cloudflare.

Local connections also verify the caller's identity and permissions. Unix socket OS permissions are not used as a substitute for global operation permissions; a permission profile for DaVinci limits the workspaces, processes, sandbox, and required credential paths.

DaVinci's agent adapters interpret the Codex app-server, Claude stream-json, and DSH/ACP protocols. Reach provides continuous process I/O and lifecycle, and does not infer the success of an LLM turn or the validity of development results.

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
| list_workspaces | Permitted workspaces | workspace_id, permission |
| list_files | File listing within a permitted area | workspace_id, relative_path |
| read_file | Read a file | workspace_id, relative_path, bounded data |
| write_file | Conditional file write | operation_id, workspace_id, path, expected_version |
| start_job | Start an asynchronous command | operation_id, workspace_id, executable, argv, limits |
| start_process_session | Start an interactive process as a job | operation_id, workspace_id, executable, argv, sandbox_profile |
| get_process_session | Query process and I/O state by session ID | process_session_id, lifecycle, output cursors |
| send_process_input | Send continuous input to stdin | process_session_id, input_id, expected_sequence, bytes |
| read_process_output | Re-fetch / incrementally fetch stdout / stderr | process_session_id, stream, cursor, limit |
| stop_process_session | Request a process stop and confirm termination | operation_id, process_session_id |
| get_operation | Reconcile an unknown acceptance result | operation_id, admission / final state |
| get_job | Current state of a job | job_id, status, timestamps |
| read_job_output | Paged output retrieval | job_id, stream, cursor, limit |
| cancel_job | Request a job stop | operation_id, job_id |

IDs of long-running jobs and process sessions are generated on Linux. Start, execution state, and output retrieval are separated so results can be looked up later. A future adapter can serve hosts that support MCP Tasks, but the internal job contract does not depend on it.

The interactive process tools are also provided to the DaVinci local client, which uses the same execution API. How fine-grained tools are exposed to AI hosts and how streaming is supported are concerns of the connection adapter; the core defines the meaning of processes and I/O.

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

For the OS execution unit of a job, systemd transient units / cgroups v2 and the like are considered, and existing processes are reconciled when the daemon restarts. **There is no guarantee that jobs continue across a reboot of Linux itself.** If the result is unknown after a reboot, it is explicitly unknown, and it is not re-executed automatically.

### Interactive process sessions (required for DaVinci's initial use)

- One process session corresponds to one execution process and job owned by Reach. Its ID is separate from DaVinci's run / agent session / turn.
- At start, the resolved executable / argv, workspace, sandbox profile, permitted environment, and the minimum required credential references are specified.
- stdin has a single write owner. A per-input `input_id` and an ordering condition reject duplicates and out-of-order input. When another writer connects, an explicit transfer of ownership / fencing is required.
- stdout and stderr are recorded locally and separately. They can be re-fetched, with limits, from monotonic offsets / cursors, so transport disconnects cause neither lost output nor implicit re-execution.
- Disconnects of WSS / MCP / DaVinci are treated as detach, separate from an explicit stop request via `stop_process_session`.
- When the DaVinci Controller restarts, it queries and reattaches to existing process sessions, and confirms the definitive result of input delivery before sending the next turn.
- That real processes survive an abnormal exit of the Reach daemon is guaranteed only once an independent OS-managed unit / supervisor is implemented and verified. Executions whose liveness cannot be confirmed are never resumed automatically.
- The MVP's base candidate is streaming I/O over pipes; PTY, FD passing, bidirectional backpressure, and per-agent handshakes are decided in detailed design and real-provider testing. Parsing agent-specific protocols is DaVinci's job.

## 7. Linux execution and sandbox

Initial target: Linux with systemd, single user, single node.

- Daemon: a non-root systemd user service. Linger is set explicitly when needed.
- Storage: local SQLite (WAL) and managed log / artifact directories.
- Files: take a workspace ID and a relative path, and enforce the boundary with safe path resolution such as openat2. Escapes via symlinks or mounts are also checked.
- Writes: conditional updates, with temporary files / atomic rename as needed.
- Commands: executable and argv are structured; no implicit shell expansion.
- Isolation: combinations of Linux namespaces, bubblewrap, systemd / cgroups v2, etc. are verified.
- Defaults: no extra mounts, secrets, network, or privileged devices. Only when DaVinci needs model APIs or dependency fetching is network access and a limited credential path allowed, in an explicit sandbox profile.
- Resources: limit CPU, memory, number of processes, execution time, and log size.

**Note:** the mere presence of bubblewrap or user namespaces does not mean sufficient isolation. Effectiveness is verified on the target distribution, configuration, and kernel features. If the required protection is unavailable, fail closed. Privileged operations are never substituted with unrestricted command execution.

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
| Linux SQLite | Indexes of operations, jobs, process_sessions, input_receipts, permissions, audit_records, artifacts | **Source of truth for operations / jobs / process sessions** |
| Linux files | Job stdout / stderr, artifacts | **Source of truth for the actual data** |
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
| Daemon crash | Reconcile OS job / process session state with SQLite. Unknown when unconfirmed. Continued execution depends on verifying independent process management |
| DaVinci restart | Reattach from the Reach job / process session IDs saved in the DaVinci run, and reconcile unconfirmed input and exits first |
| Interactive stdin delivery unknown | Reconcile receipts by input_id / sequence. Never unconditionally resend the same input under a new ID |
| Duplicate requests | Prevent double side effects with SQLite unique constraints and argument comparison |
| Permission revoked | Reject new requests. Running jobs follow an explicit stop policy |

Standard error classes (draft): node_offline / unauthorized / permission_denied / invalid_request / operation_conflict / result_unknown / sandbox_unavailable / timeout.

The availability goal is not "the WebSocket never drops" but **"even when it drops, the truth about accepted operations is never lost, and reconnection reconciles it correctly."**

## 11. MVP build order

1. **Local Core**: Go daemon, SQLite, workspaces, File / Command API, operation IDs / audit.
2. **Linux Execution**: sandbox, asynchronous jobs, **interactive process sessions and continuous I/O**, stop and ownership, reconciliation after restart.
3. **DaVinci local integration**: local API / adapter for DaVinci. Keep the existing agent protocols and development workflow, and delegate execution to Reach. Verify a local E2E.
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
- DaVinci launches Codex etc. through Reach, sends input to the same process multiple times, and re-fetches stdout/stderr in order.
- The meaning and state of DaVinci runs / agent turns / GitHub issues / draft PRs are not confused with Reach jobs.
- DaVinci Controller restarts, duplicate stdin, session stop, and provider credential separation are verified with real processes.
- The mapping between Reach job / process session IDs and DaVinci run IDs is stored in DaVinci and can be reconciled after recovery.

## 12. Items requiring design verification

| Topic | What to verify before implementation |
| --- | --- |
| Remote MCP | Compatibility of target MCP hosts and the chosen SDK with 2026-07-28 / 2025-11-25 |
| Authentication | Connection registration, revocation, and permission mapping when using an OAuth issuer / Gate |
| Node credential | Issuance, secret storage, expiry, revocation, re-registration |
| Linux sandbox | Support boundaries and fail-closed behavior of bubblewrap, namespaces, cgroups |
| Job | Recovery and stop of systemd user transient units / independent processes |
| Broker | WebSocket restoration after DO hibernation, concurrent requests, correlation on disconnect |
| MCP responses | Tool timeouts, job acceptance ACKs, reconciliation on unknown results |
| Data | Operation / job / process session schema, input receipts, retention and deletion of logs / artifacts |
| DaVinci integration | An adapter and migration plan that keep the existing run / agent session / sandbox / credential contracts |
| Interactive I/O | stdin write leases, redelivery, stdout/stderr cursors, backpressure, need for PTY |

These are decided in detailed design and verification on real environments. Design documents do not indicate running functionality or completed production acceptance.

## 13. DaVinci execution environment integration

As Reach's first client service, DaVinci uses the local API to delegate execution of agent processes, builds, tests, and verification. DaVinci's issues/PRs, agent provider protocols, runs / attempts, and judgment and acceptance of results are not moved into Reach.

DaVinci's existing Go implementation (process / sandbox / command delivery / recovery) serves as the reference implementation for design and tests and is generalized into Reach one responsibility at a time. Because the existing code has DaVinci-specific dependencies and termination conditions, wholesale copying or stopping the old controller first is not done.

DaVinci's current Cloud Run / Firestore issue projection, dashboard, and command delivery are business contracts of a separate product. What Reach's Cloudflare broker replaces is only the generic execution connection; DaVinci's business cloud features are not replaced unconditionally.

For the implementation and staged migration boundaries and acceptance items, see [DaVinci Execution Integration](docs/davinci-execution-integration.md).

## 14. Reference specifications

- [Cloudflare: Stateless MCP Handler](https://developers.cloudflare.com/agents/model-context-protocol/apis/handler-api/)
- [Cloudflare: Durable Objects WebSocket Hibernation](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [MCP specification](https://modelcontextprotocol.io/specification/)

Related: [README](README.md) / [Product Definition](PRODUCT.md) / [DaVinci Execution Integration](docs/davinci-execution-integration.md)
