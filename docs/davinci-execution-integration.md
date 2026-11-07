# DaVinci Execution Integration — Reach's first client service

- Status: **Design direction / pre-implementation and pre-migration**
- Updated: 2026-11-01
- Owners: Reach (generic Linux execution) / DaVinci (autonomous development workflow)
- Scope: local execution on one Linux node. Remote MCP follows Reach's Option B.

## 1. Adopted relationship

**Syntoria Reach is DaVinci's execution runtime.** DaVinci keeps responsibility for GitHub issues, development planning, agent sessions / turns, verification, and PRs; Reach provides, as shared capabilities, the Linux processes, sandbox, workspace boundaries, interactive I/O, jobs, logs, and recovery that this execution needs.

Reach is an independent product and can be used on its own from ChatGPT and others even when DaVinci is stopped or not installed. DaVinci becomes an execution client of Reach's local API.

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

A DaVinci run and a Reach job / process session are different things, and DaVinci stores the mapping between them. One run may include multiple jobs / process sessions.

## 2. Boundaries of authority and responsibility

| Concern | Reach owns | DaVinci owns |
| --- | --- | --- |
| Process | Launch, PID / systemd unit, isolation, exit observation, stop | Which agent / command to run and when |
| Process Session | stdin ordering and receipts, storage and cursors of stdout/stderr, write lease | Provider protocol (ACP / app-server / stream-json), context and validity of LLM turns |
| Job | Acceptance, execution lifecycle, exit code and logs, stop, reconciliation after restart | Mapping between runs / attempts and jobs, replanning of work |
| Workspace | Operable paths and mounts, access enforcement | Meaning of clone / branch / worktree, Git history, what to submit, retention policy |
| Credentials | Safe hand-off to a limited execution environment, and audit | Requesting and choosing which permissions providers / GitHub need |
| Artifacts | Retrieval of bytes, logs, hashes | Giving them meaning as results, tests, or PRs, and acceptance criteria |
| Cloud | Generic MCP, path to the Linux node | Issue state projection, reviews, dashboard and development operations |

DaVinci's agent adapters are not moved into Reach. Reach does not incorporate an agent reasoning loop, the GitHub issue model, or PR creation and submission decisions.

## 3. DaVinci's existing implementation (references and assessment)

The following are sources and design contracts checked on 2026-11-01. **This does not mean they have been migrated to or implemented in Reach.**

| Current DaVinci source | Principle Reach adopts | What must change |
| --- | --- | --- |
| [execution/process_linux.go](https://github.com/takezoh/davinci/blob/main/src/internal/execution/process_linux.go) | Go process, process group, bounded output, private log, exit confirmation | The current `Pdeathsig: SIGKILL` ties processes to the DaVinci Controller's lifetime. Reach must separately implement and verify independent job / process session survival |
| [sandbox/main.go](https://github.com/takezoh/davinci/blob/main/src/internal/sandbox/main.go) | bubblewrap, namespaces, execution HOME, credential separation | Generalize from DaVinci-specific launcher modes into per-capability Reach policies / profiles |
| [recordstore/store.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/recordstore/store.go) | Intent before persistence, atomic snapshot, fsync, reconciliation on restart | DaVinci's issue run ledger stays in DaVinci. Reach's source of truth for execution is SQLite / logs |
| [cloudnode/agent.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/cloudnode/agent.go) | Outbound connection, node credential, heartbeat, duplicate delivery reconciliation | Currently HTTPS polling. Reach Option B uses Worker / DO and WSS for generic delivery |
| [cloud/server/commands.go](https://github.com/takezoh/davinci/blob/main/src/internal/cloud/server/commands.go) | Admission, request hash, lease / fence, per-principal permissions | Do not bring issue-specific commands or repository / run generations into the Reach core |
| [DaVinci Architecture](https://github.com/takezoh/davinci/blob/main/ARCHITECTURE.md) | Separation of decisions and OS execution, unknown recovery, evidence of process stop | Connect Reach jobs and DaVinci runs / agent sessions as separate sources of truth |

The unit of migration decisions is not the package name but **the owner of the execution process, the source of truth for the state, and the responsibility of the boundary**. Only generalized capabilities are implemented in Reach; DaVinci-specific workflow stays.

## 4. Required Reach execution contract

### 4.1 Local API

When DaVinci is on the same Linux node, it connects to Reach over a Unix domain socket. It does not need to go through Cloudflare. It goes through the same operation / job / session application service and authorization as the MCP adapter.

### 4.2 Interactive processes

- `start_process_session`: start a process with an idempotent operation ID, workspace, sandbox profile, executable / argv, and resource limits. Returns a Reach job ID and process session ID.
- `get_process_session`: get the real process's liveness, exit, I/O cursors, input owner, and stored state.
- `send_process_input`: pass bytes to stdin using an `input_id` and the expected order. Duplicate input is not re-injected; the existing receipt is returned.
- `read_process_output`: get stdout and stderr with independent incremental cursors, with limits. After interruption or reconnection, they can be re-fetched from the same offset.
- `stop_process_session`: accept a stop request and reconcile evidence that the process ended on the OS side. The stop request and the stop confirmation are separate facts.
- `get_job` / `get_operation`: reconcile state after communication failures, daemon restarts, or interruptions. An unknown completion is never changed to success or failure on its own.

Simple one-off builds and tests use the regular job API. Codex and others that need continuous stdin/stdout use the process session API. The data format is provider-agnostic bytes and a structured lifecycle.

### 4.3 Writer and reconnection

In principle, only one stdin writer is allowed per session at a time. Client / owner / lease / generation are checked so that another writer cannot mix in input before an explicit transfer. Retries use the same `input_id` and payload; different content is a conflict. Delivery receipts are kept distinct from the interpretation of agent turns.

stdout/stderr can be resent from Reach's local storage by cursor. A transfer or network disconnect does not mean the process session stopped. Expiry of the log retention period is made explicit, and lost output is never treated as success.

### 4.4 Execution survival and credentials

DaVinci's existing process host has places that tie child exit to the exit of the Controller parent process. For Reach to keep real processes running while the DaVinci Controller restarts, an independent OS execution unit managed by Reach (systemd transient units, etc.) is needed. Whether they can also continue across a restart of Reach itself is verified on real Linux.

Provider tokens / refresh credentials / GitHub tokens are never placed in public MCP request bodies or stdout. How secrets are passed, limited to the scope DaVinci requests, via host-owned credential references / FDs etc., is decided in detailed design. DaVinci's current authentication and acceptance criteria are not broken.

## 5. Migration and integration plan

### Stage 1 — Reach generic execution

Implement sandbox, jobs, process sessions, local API, persistent I/O, and operation receipts in the Reach Linux runtime. DaVinci's code is used as a reference and verification baseline. The current DaVinci keeps running unchanged.

### Stage 2 — DaVinci adapter

Introduce an execution port for Reach in DaVinci's Controller / agent adapters. Store the mapping between the run's continuing state and Reach IDs in DaVinci's records. Implement first in a dedicated test environment / repository, and verify the same issue / run / PR contracts as the existing ones.

### Stage 3 — Local production cutover

Fix, per execution unit, whether the existing process host or Reach owns the current job / process session, so that no run ever has two writers. Keep the continuation and exit evidence of in-progress runs, and make the cutover targets and rollback procedure explicit before applying to production. Never swap to a different runtime in the middle of an execution on its own.

### Stage 4 — Remote transport consolidation

After Reach Option B's Workers + Durable Objects + outbound WSS has been verified E2E, the generic node execution connection DaVinci needs can be delegated to Reach. The issue projection, dashboard, and business command contracts in DaVinci's Cloud Run / Firestore are maintained and reviewed separately, and are not retired immediately just because Reach is adopted.

## 6. Acceptance

- DaVinci launches a real process such as Codex app-server from the local Reach and handles multiple turns in the same session.
- Duplicate input, out-of-order input, and writer conflicts are detected, and the same input never flows into an agent twice.
- The contents, offsets, errors, and process exit of stdout / stderr are retrieved correctly after reconnection.
- References from DaVinci runs to Reach jobs / process sessions are stored and restored.
- After the DaVinci Controller restarts, jobs from before the stop and unknown acceptance/delivery are reconciled according to confirmed facts.
- Process survival across a restart of the Reach daemon is confirmed by real measurements with an independent OS supervisor. If it cannot be confirmed, the state is `unknown` and the process is not restarted.
- DaVinci's existing contracts for issues, change requests, answers to questions, sub-issues, verification, draft PRs, and stop / recovery are maintained.
- Reach's sandbox isolates processes, workspaces, and secrets, and operations outside permissions are rejected.
- Any MCP client can use Reach without DaVinci running.
- Even with a poor remote Cloudflare connection, accepted Linux jobs / sessions and DaVinci's business state can be recovered while kept distinct.

## 7. Decisions left to detailed design

- Per real agent protocol in DaVinci: requirements for byte streams, PTY / pipe, backpressure, and subprocess FDs
- Details of the single-writer lease / generation / recovery for Reach process sessions
- The concrete systemd unit / cgroup / sandbox configuration for processes and the daemon to survive independently
- Authentication, permissions, credential injection, and revocation rules for local connections
- How much of DaVinci's workspace clone, cleanup, and Git execution moves into Reach capabilities
- How DaVinci's stored runs hold Reach IDs / sessions / input receipts
- How to avoid duplicating ownership during cutover and rollback between the current DaVinci and Reach

Related: [Reach Product](../PRODUCT.md) / [Reach Architecture](../ARCHITECTURE.md) / [DaVinci Repository](https://github.com/takezoh/davinci)
