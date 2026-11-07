# DaVinci Execution Integration — Reach's first client service

- Status: **Design direction / Reach side implemented (Stages 1–2), DaVinci not migrated**
- Updated: 2026-11-01
- Owners: Reach (generic Linux execution) / DaVinci (autonomous development workflow)
- Scope: local execution on one Linux node. Remote MCP follows Reach's Option B.

## 1. Adopted relationship

**Syntoria Reach is DaVinci's execution runtime.** DaVinci keeps responsibility for GitHub issues, development planning, prompts and the judgment of turn results, verification, and PRs; Reach provides, as shared capabilities, the Linux processes, workspace boundaries, jobs, agent sessions over Codex App Server / Claude Code stream-json / ACP, logs, and recovery that this execution needs. Sandboxing of agent tools is left to the agent harness.

Reach is an independent product and can be used on its own from ChatGPT and others even when DaVinci is stopped or not installed. DaVinci becomes an execution client of Reach's local API.

~~~mermaid
flowchart TB
    Main["DaVinci Controller: Issue / Run / evaluation / acceptance"]
    Local["Reach Local API: Unix socket"]
    Runtime["Reach Linux Runtime: process / job / agent session"]
    Harness["Agent harness: Codex App Server / Claude Code / ACP agent"]
    State[("Reach: local SQLite / logs")]
    Main <-->|"Agent Session / Job API"| Local
    Runtime <-->|"JSON-RPC over stdio"| Harness
    Local <--> Runtime
    Runtime <--> State
~~~

A DaVinci run and a Reach job / agent session are different things, and DaVinci stores the mapping between them. One run may include multiple jobs / agent sessions.

## 2. Boundaries of authority and responsibility

| Concern | Reach owns | DaVinci owns |
| --- | --- | --- |
| Process | Launch, PID / systemd unit, isolation, exit observation, stop | Which agent / command to run and when |
| Agent session | Harness process, protocol connection (Codex App Server / Claude Code stream-json / ACP), one turn at a time, event storage and cursors | Prompts and output contracts, context and validity of LLM turns |
| Job | Acceptance, execution lifecycle, exit code and logs, stop, reconciliation after restart | Mapping between runs / attempts and jobs, replanning of work |
| Workspace | Creating and deleting Orca workspaces (worktree and branch), access enforcement | What to work on in them, Git history, what to submit, retention policy |
| Credentials | Safe hand-off to a limited execution environment, and audit | Requesting and choosing which permissions providers / GitHub need |
| Artifacts | Retrieval of bytes, logs, hashes | Giving them meaning as results, tests, or PRs, and acceptance criteria |
| Cloud | Generic MCP, path to the Linux node | Issue state projection, reviews, dashboard and development operations |

The protocol connection to the harness (Codex App Server / Claude Code stream-json / ACP) moves into Reach; DaVinci's prompt building and output validation stay in DaVinci. Reach does not incorporate an agent reasoning loop, the GitHub issue model, or PR creation and submission decisions.

## 3. DaVinci's existing implementation (references and assessment)

The following are sources and design contracts checked on 2026-11-01. **This does not mean they have been migrated to or implemented in Reach.**

| Current DaVinci source | Principle Reach adopts | What must change |
| --- | --- | --- |
| [execution/process_linux.go](https://github.com/takezoh/davinci/blob/main/src/internal/execution/process_linux.go) | Go process, process group, `Pdeathsig`, bounded output, private log, exit confirmation | In Reach, children are tied to the Reach daemon instead of the DaVinci Controller, so a Controller restart does not end them |
| [sandbox/main.go](https://github.com/takezoh/davinci/blob/main/src/internal/sandbox/main.go) | Not adopted: Reach delegates sandboxing to the agent harness | DaVinci-specific isolation stays in DaVinci if it is still needed |
| [recordstore/store.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/recordstore/store.go) | Intent before persistence, atomic snapshot, fsync, reconciliation on restart | DaVinci's issue run ledger stays in DaVinci. Reach's source of truth for execution is SQLite / logs |
| [adapters/agent/codex](https://github.com/takezoh/davinci/tree/main/src/internal/adapters/agent/codex), [adapters/agent/claude](https://github.com/takezoh/davinci/tree/main/src/internal/adapters/agent/claude), [adapters/agent/dsh](https://github.com/takezoh/davinci/tree/main/src/internal/adapters/agent/dsh) | Codex App Server and ACP JSON-RPC, Claude Code stream-json with a fixed `--session-id`, one turn in flight, rejecting harness requests, ordered close | Prompt envelopes, output schemas, and DaVinci instructions stay in DaVinci |
| [cloudnode/agent.go](https://github.com/takezoh/davinci/blob/main/src/internal/adapters/cloudnode/agent.go) | Outbound connection, node credential, heartbeat, duplicate delivery reconciliation | Currently HTTPS polling. Reach Option B uses Worker / DO and WSS for generic delivery |
| [cloud/server/commands.go](https://github.com/takezoh/davinci/blob/main/src/internal/cloud/server/commands.go) | Admission, request hash, lease / fence, per-principal permissions | Do not bring issue-specific commands or repository / run generations into the Reach core |
| [DaVinci Architecture](https://github.com/takezoh/davinci/blob/main/ARCHITECTURE.md) | Separation of decisions and OS execution, unknown recovery, evidence of process stop | Connect Reach jobs and DaVinci runs / agent sessions as separate sources of truth |

The unit of migration decisions is not the package name but **the owner of the execution process, the source of truth for the state, and the responsibility of the boundary**. Only generalized capabilities are implemented in Reach; DaVinci-specific workflow stays.

## 4. Required Reach execution contract

### 4.1 Local API

When DaVinci is on the same Linux node, it connects to Reach over a Unix domain socket. It does not need to go through Cloudflare. It goes through the same operation / job / session application service and authorization as the MCP adapter.

### 4.2 Agent sessions

- `start_agent_session`: start a configured agent (Codex App Server, Claude Code, or ACP) in a workspace with an idempotent operation ID, optionally continuing the conversation of one of the caller's ended sessions (`resume_session_id`). Returns the Reach session ID and the protocol session ID.
- `send_agent_prompt`: start one turn with text. Returns the turn number without waiting. Resending the same operation ID never starts another turn; a prompt while a turn is running is rejected.
- `read_agent_events`: read agent messages, errors, and turn completion by sequence number. After interruption or reconnection, reading continues from the last sequence seen.
- `get_agent_session`: get the session state (idle, running, closed, failed, unknown) and the number of turns.
- `stop_agent_session`: interrupt any turn and end the harness.
- `get_job` / `get_operation`: reconcile state after communication failures, daemon restarts, or interruptions. An unknown completion is never changed to success or failure on its own.

Simple one-off builds and tests use the job API. Codex and ACP agents use the agent session API.

### 4.3 Turns and reconnection

Only the client that started a session can use it, and a session runs one turn at a time. Prompts are retried with the same operation ID; a different prompt under the same ID is a conflict. Turn completion recorded by Reach is a fact about the harness, not a judgment of the turn's result.

Events are kept in Reach's local storage and read by cursor. A transfer or network disconnect does not stop the session.

### 4.4 Execution survival and credentials

Harness processes belong to the Reach daemon, so they keep running while the DaVinci Controller restarts. They do not survive a restart of Reach itself: such sessions become `unknown`, and DaVinci can start a new session that continues them with `resume_session_id`.

Provider tokens / refresh credentials / GitHub tokens are never placed in public MCP request bodies or stdout. How secrets are passed, limited to the scope DaVinci requests, via host-owned credential references / FDs etc., is decided in detailed design. DaVinci's current authentication and acceptance criteria are not broken.

## 5. Migration and integration plan

### Stage 1 — Reach generic execution

Implement jobs, agent sessions, the local API, persistent events, and operation receipts in the Reach Linux runtime. DaVinci's code is used as a reference and verification baseline. The current DaVinci keeps running unchanged.

### Stage 2 — DaVinci adapter

Introduce an execution port for Reach in DaVinci's Controller that replaces its harness process and protocol clients with Reach agent sessions. Store the mapping between the run's continuing state and Reach IDs in DaVinci's records. Implement first in a dedicated test environment / repository, and verify the same issue / run / PR contracts as the existing ones.

### Stage 3 — Local production cutover

Fix, per execution unit, whether the existing process host or Reach owns the current job / agent session, so that no run ever has two owners. Keep the continuation and exit evidence of in-progress runs, and make the cutover targets and rollback procedure explicit before applying to production. Never swap to a different runtime in the middle of an execution on its own.

### Stage 4 — Remote transport consolidation

After Reach Option B's Workers + Durable Objects + outbound WSS has been verified E2E, the generic node execution connection DaVinci needs can be delegated to Reach. The issue projection, dashboard, and business command contracts in DaVinci's Cloud Run / Firestore are maintained and reviewed separately, and are not retired immediately just because Reach is adopted.

## 6. Acceptance

- DaVinci launches a real process such as Codex app-server from the local Reach and handles multiple turns in the same session.
- Duplicate prompts and concurrent turns are rejected, and the same prompt never reaches an agent twice.
- Events, their order, errors, and the session's end are read correctly after reconnection.
- References from DaVinci runs to Reach jobs / agent sessions are stored and restored.
- After the DaVinci Controller restarts, jobs from before the stop and unknown acceptance/delivery are reconciled according to confirmed facts.
- After a restart of the Reach daemon, sessions are `unknown` and are not restarted automatically.
- DaVinci's existing contracts for issues, change requests, answers to questions, sub-issues, verification, draft PRs, and stop / recovery are maintained.
- The harness sandbox confines agent tools (Codex sandbox mode, Claude Code permission and sandbox settings, ACP permission handling), and Reach rejects operations outside permissions.
- Any MCP client can use Reach without DaVinci running.
- Even with a poor remote Cloudflare connection, accepted Linux jobs / sessions and DaVinci's business state can be recovered while kept distinct.

## 7. Decisions left to detailed design

- How DaVinci's output schemas and developer instructions are passed through Reach agent sessions
- Which agent events DaVinci needs beyond messages, errors, and turn completion (structured output, usage)
- Authentication, permissions, credential injection, and revocation rules for local connections
- How much of DaVinci's Git execution moves into Reach capabilities (workspace creation and cleanup are Reach's, through Orca)
- How DaVinci's stored runs hold Reach job / session IDs and the last event sequence
- How to avoid duplicating ownership during cutover and rollback between the current DaVinci and Reach

Related: [Reach Product](../PRODUCT.md) / [Reach Architecture](../ARCHITECTURE.md) / [DaVinci Repository](https://github.com/takezoh/davinci)
