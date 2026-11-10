# Syntoria Reach — Product Definition

- Status: **Direction decided / Stages 1–2 implemented**
- Updated: 2026-11-08
- Initial OS: **Linux**
- Connection architecture: **Option B — Cloudflare Workers + Durable Objects + Linux Connector**

## 1. Product definition

**Give your AI the ability to act in your environment.**

Syntoria Reach is an independent service that lets ChatGPT, Claude, Codex, and other AIs safely use the capabilities of Linux PCs and servers that the user owns or manages. Through standard MCP, an AI can operate on files, run commands, and manage long-running work as jobs.

The AI decides and plans. Reach provides connectivity, access control, operations, execution state, results, and audit information. Embedding an LLM in Reach is not a base requirement.

**Reach is an execution environment and an interface to agents.** Programs that drive agents, **Controllers** such as a development workflow, run on top of it: Reach starts them and lets remote AIs call their tools, and they use the node's workspaces and agents through Reach. Reach does not know what a Controller does, and AIs can use Reach on their own.

## 2. Value for people

| Situation | What the user gets |
| --- | --- |
| Requests while away | Ask an AI on a smartphone and have it investigate, build, and test in their own Linux development environment |
| Using the existing environment | Let AI use repositories, files, tools, and private environments without relocating them |
| Switching AIs | Reuse the execution environment when moving from ChatGPT to another MCP client |
| Long-running work | Keep already-running jobs when the connection drops, and query their state, logs, and results later |
| Autonomous work | A Controller runs multiple agents, builds, and verifications on Reach's execution environment, while its own process and judgment of results stay with the Controller |
| Safe delegation | Configure targets, workspaces, and permissions per AI, and trace what was done |

The goal is to reduce the effort of remotely operating one's own environment and the effort of relocating or reconfiguring environments so that AI can work in them.

## 3. User experience

### Case A: Files and commands

1. The user sets up Reach on each of their Linux machines (nodes), next to the Orca server or without it, and selects the operations, commands and agents to allow, each agent with its role.
2. The AI lists the nodes with Reach's MCP tools, with the agents each one can run and their roles, picks the node for the work, then continues ongoing work in its workspace or creates a workspace for new work. Reach creates it on that node (on Orca, as an Orca project).
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

### Case D: A Controller's execution environment

1. A remote AI chooses a node and a Controller by its role and calls one of the Controller's tools; Reach forwards the call to the Controller it runs on that node.
2. The Controller creates a workspace for its work and asks Reach's local API to start an agent session there. Reach starts the agent (for example Claude Code) in an Orca terminal of the workspace, where the user can watch it, or, on a node without Orca, as its own process; the agent sandboxes its own tools.
3. The Controller sends prompts and judges the meaning of each turn's result. Reach delivers the prompts, reports the agent's state and reply, and tracks process and job state.
4. The Controller binds Reach's workspace, agent session and job IDs to its own work. When it restarts, it re-reads Reach's current state and reconciles unknown prompt delivery before continuing. The Controller decides when its work is done.

## 4. Capabilities

| Capability | MVP | Description |
| --- | --- | --- |
| Node discovery | Yes | The person's nodes, their connection state, and the agents each one can run with their roles |
| Workspace management | Yes | One workspace per unit of work: create, list and annotate (note, status) workspaces, each a folder of its own named `reach-*`, kept by Orca as a project or by Reach as a local workspace; completed workspaces are removed after a week without activity |
| File access | Yes | List, read, conditional write |
| Command execution | Yes | Isolated execution of permitted Linux commands |
| Asynchronous jobs | Yes | Start, get state/output, stop |
| Agent sessions | Yes | Agents in Orca terminals, or as Reach's processes on nodes without Orca: start, prompt, read the reply, stop. The agent sandboxes its own tools |
| Authentication and permissions | Yes | Permission per AI client, node, workspace, and operation |
| Audit | Yes | Records of actor, target, time, and result |
| Browser / Desktop GUI | Later | Browser and screen operation |
| Multi-node | Yes (Linux) | One remote MCP endpoint for all of the person's Linux nodes; other OSes later |
| Agent-specific orchestration | Later | Per-agent planning and conversation management. Starting agents in Orca terminals is supported in the MVP |
| P2P / WebRTC | Later | Direct communication paths for clients that support them |
| MCP Events | Later | Asynchronous notifications such as job completion and node state |

## 5. Core concepts

- **Node**: a computer being operated, a Linux machine running Reach. A person may connect several to one gateway; each has its own workspaces and its own agents.
- **Workspace**: one unit of work on a node: a folder of its own named `reach-*`, a note saying what the work is and a status, kept as an Orca project or by Reach itself; the scope of files and operations of a request.
- **Capability**: an ability Reach provides, such as File, Command, or Job.
- **Client / Connection**: an authenticated AI client and its connection.
- **Operation**: an individual operation request. It has a unique identifier used to detect resends and duplicates.
- **Job**: an asynchronous execution and its state, independent of the communication session.
- **Agent**: an agent a node may run, such as a coding agent or a reviewer, registered with its role: what it is for, by which an AI chooses it.
- **Controller**: a program a node runs, or connects to, as an MCP server for remote callers, registered with its role; it drives agents through Reach's local API.
- **Agent Session**: an agent running in a workspace (in an Orca terminal, or as Reach's process), which a caller prompts and whose replies it reads through Reach. Distinct from MCP sessions and from a caller's own run.
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

### Division of responsibilities with Controllers

| Item | Reach | Controller |
| --- | --- | --- |
| Linux processes / jobs | Launch, state, stop / reconnect / logs | Instructions, interpretation of results |
| Agent sessions | Starting the agent (in an Orca terminal or as its process), relaying prompts, reporting its state and reply, stopping it | Prompts, output contracts, judging turn results |
| Workspace | Creating workspaces; permitted paths | What to work on in them; the meaning of their contents |
| Authorization | Final enforcement for node / workspace / capability / operation, and which callers may call which Controller tools | Its own requests and their purposes |
| Work state | Only the execution facts of operations, jobs and agent sessions | Its own units of work, results and acceptance |
| Remote connection | Generic MCP / node transport, forwarding calls to Controller tools | What its tools do |

A Controller's unit of work and a Reach job are separate entities; one unit of work can use multiple Reach jobs and agent sessions. Reach works without knowing what any Controller does.

## 7. Remote delivery (adopted: Option B)

- **Cloudflare Workers**: stable entry point for remote MCP; authentication, request validation, routing.
- **Cloudflare Durable Objects**: per-node connection broker that relays the WebSocket with the Linux connector.
- **Linux Connector / Runtime**: a single resident Go service. It connects over outbound WSS and manages operations and jobs.
- **Linux SQLite**: persists operations, jobs, permissions, audit, and result references.

No always-on VM or container is provisioned in the cloud. Durable Objects are designed so that WebSocket Hibernation can be used. To stay reachable remotely, the Linux connector stays resident.

The initial specification does not include an MCP tool that creates P2P connections. It does not assume that AIs can use arbitrary P2P connections directly; regular MCP tools are relayed through the gateway. Direct communication is a candidate for a later transport adapter.

## 8. Safety and user control

- Evaluate permissions per node, workspace, capability, and operation.
- Agents run with their own sandbox, in Orca terminals or as Reach's processes. Commands and jobs run only from the workspace allowlist, without a shell and with a minimal environment.
- Authentication and operation permissions are verified on Reach's execution side. AI input and external documents are never grounds for permission.
- Allow requiring additional user approval for important operations.
- Do not retry commands without limit when the connection drops.
- Provide ways to review operation history and results.

## 9. MVP scope and completion criteria

Target: **Linux with systemd, one user, one or more nodes.** The minimum Linux distributions and kernel versions are decided by verification.

In the MVP, an MCP client connecting from the cloud can perform file operations and run commands in permitted Linux workspaces. Long-running work becomes jobs and is still managed on Linux after the MCP connection drops. Completion criteria: duplicate execution is prevented by operation ID, access outside permissions is denied, offline state is reported explicitly, and history and results can be retrieved again.

**Additional MVP acceptance criteria: agent sessions and Controllers.** A Controller, connected locally, can create workspaces, start agents such as Codex through Reach, run multiple turns in one session, and handle stop and reconnection; remote AIs can call its tools through the gateway within the permissions of the remote client.

## 10. Non-goals and evaluation

The initial release does not cover general-purpose AI agent orchestration, LLM hosting, Weave workflow processing, WebRTC P2P, desktop GUI, or automatic scheduling of offline jobs. Reach runs agent sessions (agents in Orca terminals or as its own processes), but prompts, plans, and judging results stay with the caller.

Metrics: execution success rate, unknown-result rate, job reconciliation success rate after disconnects, prevention of duplicate execution, accuracy of permission denials, manual effort required from the user, and compatibility across different AI clients.

Related: [README](README.md) / [Technical Architecture](ARCHITECTURE.md)
