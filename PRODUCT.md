# Syntoria Reach — Product Definition

- Status: **Direction decided / Stages 1–2 implemented**
- Updated: 2026-11-01
- Initial OS: **Linux**
- Connection architecture: **Option B — Cloudflare Workers + Durable Objects + Linux Connector**

## 1. Product definition

**Give your AI the ability to act in your environment.**

Syntoria Reach is an independent service that lets ChatGPT, Claude, Codex, and other AIs safely use the capabilities of Linux PCs and servers that the user owns or manages. Through standard MCP, an AI can operate on files, run commands, and manage long-running work as jobs.

The AI decides and plans. Reach provides connectivity, access control, operations, execution state, results, and audit information. Embedding an LLM in Reach is not a base requirement.

**The first full-scale client service is DaVinci.** DaVinci directs the development process and agents, and Reach serves as its Linux execution runtime. Reach is not an internal component of DaVinci; it is an independent service that other AIs can use on their own.

## 2. Value for people

| Situation | What the user gets |
| --- | --- |
| Requests while away | Ask an AI on a smartphone and have it investigate, build, and test in their own Linux development environment |
| Using the existing environment | Let AI use repositories, files, tools, and private environments without relocating them |
| Switching AIs | Reuse the execution environment when moving from ChatGPT to another MCP client |
| Long-running work | Keep already-running jobs when the connection drops, and query their state, logs, and results later |
| Autonomous development | DaVinci safely runs multiple agents, builds, and verifications on Reach's execution environment, while the development process stays managed by DaVinci |
| Safe delegation | Configure targets, workspaces, and permissions per AI, and trace what was done |

The goal is to reduce the effort of remotely operating one's own environment and the effort of relocating or reconfiguring environments so that AI can work in them.

## 3. User experience

### Case A: Files and commands

1. The user sets up Reach on Linux next to the Orca server, and selects the operations, commands and agents to allow.
2. The AI uses Reach's MCP tools to check the target node and available capabilities, then continues ongoing work in its workspace or creates a workspace for new work. Reach creates it on the Orca server.
3. The AI reads permitted files and runs the commands it needs.
4. Reach returns operation results and records the caller, target, and result.
5. The AI makes its next decision or operation as needed.

### Case B: Asynchronous jobs

1. The AI starts a build or test as a job.
2. Reach checks the request's uniqueness and records the accepted job locally.
3. Even if the AI ends or disconnects, the accepted job keeps running on Linux.
4. After reconnecting, the AI gets the state, logs, and results by job ID.
5. Acceptance of a stop request is shown separately from the fact that the job actually stopped.

### Case C: Node offline

If the target node is offline, Reach reports it as unavailable. In the MVP, requests are not automatically held or accumulated and later executed on their own.

### Case D: DaVinci's execution runtime

1. DaVinci decides the work from the issue's goal and chooses the workspace purpose and which Codex / Claude Code / DSH etc. to run.
2. DaVinci asks Reach's local API to start an agent session, and Reach launches the harness (Codex App Server, Claude Code, or an ACP agent such as DSH), which sandboxes its own tools.
3. DaVinci sends prompts and judges the meaning of each turn's result. Reach delivers turns, records the agent's events, and tracks process and job state.
4. DaVinci binds Reach's agent session / job IDs to its own run, supervises progress, and receives verification results.
5. When DaVinci restarts, it re-fetches Reach's current state and checks for unknown exits and unknown prompt delivery before continuing. DaVinci decides completion of the original issue and PR submission.

## 4. Capabilities

| Capability | MVP | Description |
| --- | --- | --- |
| Node discovery | Yes | Available nodes and their connection state |
| Workspace management | Yes | One workspace per unit of work: create, list and annotate (note, status) workspaces, each an Orca project named `reach-*` with a folder of its own; Orca holds their state, and completed workspaces are removed after a week without activity |
| File access | Yes | List, read, conditional write |
| Command execution | Yes | Isolated execution of permitted Linux commands |
| Asynchronous jobs | Yes | Start, get state/output, stop |
| Agent sessions | Yes | Codex App Server / Claude Code / ACP agents: start, one turn at a time, events, stop. The harness sandboxes its own tools |
| Authentication and permissions | Yes | Permission per AI client, node, workspace, and operation |
| Audit | Yes | Records of actor, target, time, and result |
| Browser / Desktop GUI | Later | Browser and screen operation |
| Multi-node | Later | Extension to multiple Linux machines and other OSes |
| Agent-specific orchestration | Later | Per-agent planning and conversation management. Starting Codex App Server / Claude Code / ACP agents is supported in the MVP |
| P2P / WebRTC | Later | Direct communication paths for clients that support them |
| MCP Events | Later | Asynchronous notifications such as job completion and node state |

## 5. Core concepts

- **Node**: the computer being operated. Initially one Linux node.
- **Workspace**: one unit of work: an Orca project named `reach-*` with a folder of its own, a note saying what the work is and Orca's workspace status; the scope of files and operations of a request.
- **Capability**: an ability Reach provides, such as File, Command, or Job.
- **Client / Connection**: an authenticated AI client and its connection.
- **Operation**: an individual operation request. It has a unique identifier used to detect resends and duplicates.
- **Job**: an asynchronous execution and its state, independent of the communication session.
- **Agent Session**: a running agent harness (Codex App Server, Claude Code, or an ACP agent) and its protocol session, driven one turn at a time. Distinct from MCP sessions and from a caller's own run.
- **Artifact**: a reference to output logs or generated files.
- **Permission**: a definition of what is allowed for an actor, target, and operation.

## 6. Service boundaries and independence

Reach is an independent service and does not require Weave, Lore, Trace, or any particular LLM to be running.

| Service | Responsibility |
| --- | --- |
| Lore | Knowledge and long-term memory |
| Weave | Actions, collaboration, shared human/AI state, approvals |
| Trace | Records of activity, conversations, etc. |
| Reach | Access to and operation of execution environments, job management |

When Reach is used based on an action approved in Weave, the AI / orchestrator / adapter combines the two services. Reach does not make Weave action IDs or approval states a required part of its internal model.

A Reach job completing does not mean a Weave action has completed. The calling side owns that judgment and its reflection.

### Division of responsibilities with DaVinci

| Item | Reach | DaVinci |
| --- | --- | --- |
| Linux processes / jobs | Launch, isolation, state, stop / reconnect / logs | Instructions, interpretation of results |
| Agent sessions | Harness process, protocol connection (Codex App Server / Claude Code stream-json / ACP), turns and events, ownership | Prompts, output contracts, judging turn results |
| Workspace | Permitted areas and runtime mount / path restrictions | Work targets; meaning and management of clones, branches, and results |
| Authorization | Final enforcement for node / workspace / capability / operation | Managing development requests, execution purposes, and assignees |
| Development runs | Only the execution facts of jobs / agent sessions | Runs / attempts, issues, sub-issues, PRs, verification and acceptance |
| Cloud connection | Generic MCP / node transport | DaVinci-specific dashboard, issue projection, reporting |

A DaVinci run and a Reach job are separate entities; one run can use multiple Reach jobs / agent sessions. DaVinci becomes a client service that delegates Linux execution to Reach, but Reach itself works without knowing about DaVinci. The existing DaVinci runtime environment is kept until verification and migration are complete.

[DaVinci integration](docs/davinci-execution-integration.md)

## 7. Remote delivery (adopted: Option B)

- **Cloudflare Workers**: stable entry point for remote MCP; authentication, request validation, routing.
- **Cloudflare Durable Objects**: per-node connection broker that relays the WebSocket with the Linux connector.
- **Linux Connector / Runtime**: a single resident Go service. It connects over outbound WSS and manages operations and jobs.
- **Linux SQLite**: persists operations, jobs, permissions, audit, and result references.

No always-on VM or container is provisioned in the cloud. Durable Objects are designed so that WebSocket Hibernation can be used. To stay reachable remotely, the Linux connector stays resident.

The initial specification does not include an MCP tool that creates P2P connections. It does not assume that AIs can use arbitrary P2P connections directly; regular MCP tools are relayed through the gateway. Direct communication is a candidate for a later transport adapter.

## 8. Safety and user control

- Evaluate permissions per node, workspace, capability, and operation.
- Agents run inside their harness's sandbox. Commands and jobs run only from the workspace allowlist, without a shell and with a minimal environment.
- Authentication and operation permissions are verified on Reach's execution side. AI input and external documents are never grounds for permission.
- Allow requiring additional user approval for important operations.
- Do not retry commands without limit when the connection drops.
- Provide ways to review operation history and results.

## 9. MVP scope and completion criteria

Target: **Linux with systemd, one user, one node.** The minimum Linux distributions and kernel versions are decided by verification.

In the MVP, an MCP client connecting from the cloud can perform file operations and run commands in permitted Linux workspaces. Long-running work becomes jobs and is still managed on Linux after the MCP connection drops. Completion criteria: duplicate execution is prevented by operation ID, access outside permissions is denied, offline state is reported explicitly, and history and results can be retrieved again.

**Additional MVP acceptance criteria: agent sessions and real use by DaVinci.** DaVinci, connected locally, can start agents such as Codex through Reach, run multiple turns in one session, and handle stop, reconnection, and events. An E2E verification is performed that keeps DaVinci's existing run / verification / PR contracts. Completing the migration of DaVinci's production environment itself is a separate, staged cutover acceptance.

## 10. Non-goals and evaluation

The initial release does not cover general-purpose AI agent orchestration, LLM hosting, Weave workflow processing, WebRTC P2P, desktop GUI, or automatic scheduling of offline jobs. Reach runs agent sessions over Codex App Server / Claude Code stream-json / ACP, but prompts, plans, and judging results stay with the caller.

Metrics: execution success rate, unknown-result rate, job reconciliation success rate after disconnects, prevention of duplicate execution, accuracy of permission denials, manual effort required from the user, and compatibility across different AI clients.

Related: [README](README.md) / [Technical Architecture](ARCHITECTURE.md) / [DaVinci Integration](docs/davinci-execution-integration.md)
