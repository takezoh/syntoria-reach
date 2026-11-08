# Syntoria Reach

**Give your AI the ability to act in your environment.**

Syntoria Reach is an independent MCP service that lets AIs such as ChatGPT, Claude, and Codex safely use the files, commands, and jobs in a user's Linux environment.

The AI decides and plans; Reach handles authentication, permission checks, execution, and result retrieval. Reach itself does not need to embed an LLM.

**DaVinci is the first full-scale service built on Reach.** DaVinci owns development planning, agent sessions, issues/PRs, and verification and acceptance of results, while Reach runs and manages Linux processes, asynchronous jobs, and agent sessions (agents it starts in Orca terminals, or as its own processes on a machine without Orca). Reach can be used on its own; DaVinci does not have to be running.

## What users can do

- Access permitted workspaces on their own Linux PCs and development servers from an AI while away from them, through one remote MCP endpoint for all of their machines (nodes)
- Register the agents each machine may run, each with its role (for example a coding agent, or DaVinci), so that an AI picks the machine and the agent for the work
- Read and write files, and run commands, builds, and tests
- Start long-running work as a job and check its progress and results later, even if the connection to the AI drops
- Run agents such as Codex in their own sandbox and drive them turn by turn, from DaVinci or any other client
- Use the same environment's capabilities from multiple AI clients
- Control execution permissions per caller, workspace, and operation type

## Adopted architecture

~~~mermaid
flowchart TB
    AI["ChatGPT / Claude / Codex / other MCP clients"]
    W["Cloudflare Workers: authenticated MCP Gateway"]
    DO["Durable Objects: per-node connection broker"]
    L["Linux Reach on each node: Go Connector + Runtime"]
    DB[("Linux SQLite: operations / jobs / audit")]
    OS["Workspace / files / commands / jobs; agents in Orca terminals or as Reach's processes"]
    DV["DaVinci Controller: development and agent orchestration"]
    AI <-->|"MCP Streamable HTTP"| W
    W <--> DO
    DO <-->|"Outbound WSS from Linux"| L
    L <--> DB
    L --> OS
    DV <-->|"Local Reach API (Unix socket)"| L
~~~

**No always-on VM or container is needed on the Cloudflare side.** Workers accept MCP requests and Durable Objects broker connections. On Linux, a lightweight Reach process keeps the WebSocket open and holds operation and job state locally.

If the connection to Cloudflare drops, accepted jobs keep running and the source of truth for their state stays on Linux. If the node is offline, the initial version reports it as unavailable instead of queueing requests.

## Place within Syntoria

- **Lore**: long-term memory and knowledge
- **Weave**: shared state between humans and AI; requests, approvals, and results
- **Trace**: records of activity and conversations
- **Reach**: gives AI the capabilities of an execution environment

Each service can be used independently. When a Weave MCP event should trigger a Reach operation, the AI or an external integration component combines the two.

## Initial scope

Linux, one user, one or more nodes: file operations, permitted command execution, asynchronous jobs, **agent sessions**, permission management, operation history, and remote MCP connections. A local end-to-end run in which DaVinci uses Reach as its execution runtime is part of the initial acceptance criteria. Browser/GUI operation and WebRTC P2P are candidates for later extensions.

So far, **Stages 1 (Local Core) and 2 (Linux Execution) of ARCHITECTURE.md §11 are implemented, and Stage 4 (Remote MCP) in part**: remote MCP clients that the owner approved in Syntoria Gate can list the owner's nodes with their agents and roles, and use workspaces and agent sessions on each node. Everything else described here is design and does not indicate implemented functionality.

## Running Reach (Stages 1–2)

A single Go daemon, `reach`. It serves the local API as HTTP/JSON over a Unix socket and stores operations, jobs, and audit records in SQLite. Job output is kept next to the database.

A workspace is one unit of work: a folder of its own named `reach-<name>` under `workspaces`, with a note saying what the work is and a status (todo, in-progress, in-review, completed). It starts empty; the work brings in what it needs. A node keeps its workspaces and runs its agents with one of two workspace backends, chosen by its configuration: on the [Orca](https://www.onorca.dev/) server of the same host when `orca` is set, or as local workspaces otherwise. Both serve the same methods with the same arguments and results.

**On Orca.** Workspaces live on the Orca server of the same host. A workspace is one unit of work: a folder of its own under `workspaces`, registered in Orca as a folder project named `reach-<name>`, with a note saying what the work is (the comment of the project's root entry) and Orca's workspace status (todo, in-progress, in-review, completed). It starts empty; the work brings in what it needs. Orca is its only record: Reach keeps nothing about workspaces, reads them from Orca on every request, and sees only Orca projects whose name starts with `reach-`. Its ID is the Orca project's repo ID, and a client's permissions apply to every workspace. A workspace that is `completed` and has had no activity (in Orca, or through Reach) for 7 days is removed, Orca project and folder, by the daemon, which checks every hour; callers do not delete workspaces. A project removed in Orca leaves its folder, which the daemon removes once nothing in it has changed for 7 days. Callers never talk to Orca themselves: they use Reach's methods, and Reach drives Orca.

Agents run on Orca too. `start_agent_session` types the agent's configured command into a new Orca terminal of the workspace, where the user can watch and use it. Prompts are typed into the terminal (`orca terminal send`), and replies are read from Orca's agent status (`orca worktree ps`): the agent's last message of each turn, which Orca's agent status hooks report. The session ID is the terminal's Orca handle; Reach keeps nothing about sessions, which outlive the daemon and end when the terminal closes, and every terminal in a Reach workspace is a session. Claude Code must not stop at its folder trust prompt or its auto mode setup prompt in a workspace: trust the workspaces folder and dismiss that prompt in `~/.claude.json` (the `agent-claude` module of Agent Environments does both). Orca's agent status hooks must be installed for the agent (`orca agent hooks status`).

**Without Orca (local workspaces).** Reach keeps each workspace itself: the folder `reach-<name>` under `workspaces`, and a record of its note and status in its database. Its ID is the record's. Completed workspaces are removed as on Orca, folder and record; a folder `reach-*` without a record (left by a daemon that stopped while creating it) goes once nothing in it has changed for 7 days. `start_agent_session` runs the agent's command as a process of the daemon in the workspace folder and talks to it over the agent's `protocol`, as DaVinci does: `claude` (Claude Code over stream-json; Reach adds the stream-json flags and a session ID), `codex` (Codex App Server; approvals are off) or `acp` (Agent Client Protocol, such as DSH; permission requests are cancelled). The agent gets the fixed environment of commands plus its `env`, and its own settings and sandbox decide what it may do, since no one answers its questions. A prompt starts a turn; the reply is the agent's last message of the turn (or the error that ended it), of which Reach keeps the first 64 KiB. Sessions are kept in memory: the agents are children of the daemon, end with it, and their sessions are then `not_found`. The agent's stderr is kept in `agents/<session>.stderr` next to the database until the session is stopped.

The implementation lives in `src/` (Go module `github.com/takezoh/syntoria-reach/src`). To install it as a systemd user service, give [INSTALL.md](INSTALL.md) to a coding agent on the host; it is a prompt that checks the prerequisites, agrees on the configuration with you, and installs and verifies the service. The manual steps are:

~~~sh
cd src && go build -o ~/.local/bin/reach ./cmd/reach
chmod 600 ~/.config/reach/config.json          # contains tokens; must not be readable by group/other
reach -config ~/.config/reach/config.json      # systemd user unit: deploy/systemd/reach.service
~~~

~~~json
{
  "orca": ["/home/me/.local/bin/orca-ide"],
  "agents": [
    {"id": "claude", "role": "Develops and reviews code in a workspace",
     "command": ["/home/me/.local/bin/claude", "--model", "claude-opus-5-5", "--effort", "xhigh"]}
  ],
  "commands": ["/usr/bin/make", "/usr/bin/go", "/usr/bin/git"],
  "clients": [{"id": "davinci", "token": "<random string of at least 32 characters>",
               "allow": ["workspace.create", "file.read", "file.write", "command.run", "agent.run"]}]
}
~~~

`socket` defaults to `$XDG_RUNTIME_DIR/reach/reach.sock`, `database` to `$XDG_STATE_HOME/reach/reach.db`, and `workspaces`, the folder holding the workspace folders, to `workspaces` next to the database. A client's `allow` applies to every workspace; `workspace.create` allows creating and updating them. `commands` are the executables `run_command` and `start_job` may run. Every agent may be started in every workspace. Its `role` says what it is for, and callers, such as remote AIs choosing a node and an agent, see it with the agent's ID. On Orca, its `command` is typed into the terminal's shell, each argument quoted, so model, effort and permission flags belong there. The configuration names no repositories or projects: workspaces are created by callers.

Without `orca`, each agent also names its `protocol`, and may have an `env`:

~~~json
{
  "agents": [
    {"id": "coder", "role": "Develops and reviews code in a workspace", "protocol": "claude",
     "command": ["/home/me/.local/bin/claude", "--model", "claude-opus-5-5", "--effort", "xhigh"]},
    {"id": "codex", "role": "Implements a change and runs its tests", "protocol": "codex",
     "command": ["/usr/local/bin/codex", "app-server", "-c", "sandbox_mode=\"workspace-write\""], "env": {"CODEX_HOME": "/home/me/.codex"}}
  ]
}
~~~

Requests are JSON sent to `POST /v1/<method>` with `Authorization: Bearer <token>`. The caller must run as the same UID as the daemon (checked with SO_PEERCRED).

| method | Description |
| --- | --- |
| `list_capabilities` | Your permissions, and the commands and agents (each `id` with its `role`) you may run (with the permission to run them) |
| `create_workspace` | `operation_id`, `name`, `note`. Creates the folder `reach-<name>` with the note (on Orca, registered as a folder project); returns the workspace (`workspace` ID, `name`, `note`, `status`, `path`) |
| `list_workspaces` | Every workspace, by name |
| `update_workspace` | `operation_id`, `workspace`, `note` and/or `status`. Changes the note and status (on Orca, the comment and workspace status, where Orca decides which statuses exist); `completed` makes the workspace due for removal after 7 days without activity |
| `list_files` | Directory listing for `workspace`, `path` |
| `read_file` | `workspace`, `path`, `offset`, `limit`. Returns `data` (base64) and `version` |
| `write_file` | `operation_id`, `workspace`, `path`, `content` (base64), `expected_version` (empty means create only). Replaces the file by atomic rename |
| `run_command` | `operation_id`, `workspace`, `executable` (absolute path from the allowlist), `args`, `timeout_seconds`. Runs synchronously in the workspace root without a shell |
| `get_operation` | Looks up an operation's state and result by `operation_id` |
| `start_job` | `operation_id`, `workspace`, `executable`, `args`, `timeout_seconds` (default 1 h). Starts the command in the background; the result is the job |
| `get_job` | `job_id`. State (`running`, `succeeded`, `failed`, `cancelled`, `unknown`), exit code |
| `read_job_output` | `job_id`, `stream` (`stdout`/`stderr`), `offset`, `limit`. `eof` is true once the job ended and the end was read |
| `cancel_job` | `operation_id`, `job_id`. Kills the job's process group |
| `start_agent_session` | `operation_id`, `workspace`, `agent`. Starts the agent in a new Orca terminal of the workspace (without Orca, as a process in the workspace folder) and returns once the agent waits for its first prompt, or after 15 s; the result is the session as `get_agent_session` returns it. `harness_unavailable`, with no session left, if the agent does not start (on Orca, if no agent runs in the terminal by then). A `completed` workspace is set back to `in-progress` |
| `send_agent_prompt` | `operation_id`, `session_id`, `text`. Delivers the prompt (on Orca, types it into the terminal) and returns once the agent started its turn (`harness_unavailable` if it did not, for example while it waits for input). Send it while the session's `state` is `idle`; otherwise it fails with `invalid_state`: during a turn, while Orca sees the agent waiting for its user (such as a permission), which the user answers in Orca, or cannot tell, and once the agent quit. `text` is at most 64 KiB, has no control characters other than newlines and tabs, and does not start with `!` or `/`, which Claude Code runs as a shell command or one of its commands: those would do more than `agent.run` allows |
| `read_agent_output` | `session_id`. The agent's `state` as in `get_agent_session` and, once it is `idle`, `reply`: the agent's last message of its turn (on Orca, as Orca's agent status reports it), empty before the first prompt. Orca keeps the first 8,000 characters (UTF-16 code units) of a reply, local workspaces the first 64 KiB; `reply_truncated` is true when the reply reached that length and its end may be cut off. `not_found` once the session has ended |
| `get_agent_session` | `session_id`. The session as the node observes it: `workspace`, `agent` (on Orca, the agent Orca sees in the terminal), and `state`: `idle` (it waits for a prompt), `working`, `waiting_on_human` (on Orca: Orca sees it ask its user something, or cannot tell; `waiting_on_human` says what Orca saw) or `exited` (no agent runs any more) |
| `stop_agent_session` | `operation_id`, `session_id`. Ends the agent and the session (on Orca, closes the terminal); its reply is no longer readable |

- Requests with side effects are recorded as operations. Resending the same `operation_id` with the same arguments returns the existing result; different arguments return `operation_conflict`.
- Commands and jobs are children of the daemon and die with it. Operations and jobs that were running when the daemon stopped become `unknown` at the next start. They are never re-executed automatically. Agents run in Orca terminals and keep running; the agents of local workspaces are children of the daemon and die with it.
- Sandboxing: an agent runs with its own settings and sandbox (for Claude Code, `~/.claude/settings.json` and the flags in its `command`): on Orca in the user's login shell in its terminal, without Orca as a process of the daemon. Reach does not sandbox commands, jobs or agents: they run as the daemon's user, commands and jobs restricted to the allowlist and a minimal environment.

## Remote MCP (Stage 4, in part)

The gateway in `cloud/` is a Cloudflare Worker: a remote MCP endpoint (MCP Streamable HTTP, stateless) that accepts [Syntoria Gate](https://github.com/takezoh/syntoria-gate) access tokens, for all of the person's nodes, and a Durable Object per node that holds the WebSocket the node's daemon opens to it. With `remote` in its config, the daemon dials out to the gateway, so no inbound port is needed, and serves the requests that arrive there with the same methods as the local API.

~~~json
"remote": {"url": "wss://reach-api.<parent>/connect", "token": "<the node's token>", "client": "remote"}
~~~

- **Nodes**: the gateway's Worker secret `NODES` registers them, as a JSON object of node IDs and their tokens, `{"desktop": "<token>", "build-server": "<token>"}`. Each token is at least 32 characters and belongs to one node; a daemon sends only its token, and the gateway knows the node by it. Adding an entry registers a node; removing it, or changing its token, cuts that node off.
- **Tools**: `list_nodes` lists every node by ID, whether it is connected (`online`), and, for a connected node, the agents the remote client may start there with their roles (`{"nodes": [{"node", "online", "agents": [{"id", "role"}]}]}`, from each node's `list_capabilities`). An AI chooses the node and the agent for the work from it. Every other tool is a local API method of a node: `list_capabilities`, `list_workspaces`, `create_workspace`, `update_workspace`, `start_agent_session`, `send_agent_prompt`, `read_agent_output`, `get_agent_session`, `stop_agent_session`, `get_operation`. Each takes a required `node`, which the gateway removes before it forwards the call to that node; the other arguments and the results are those of the method. Workspaces, sessions and operations belong to their node. An unknown node is `not_found`, and Reach errors come back as tool errors (`{"error": {"code", "message"}}`).
- **Who may connect**: Gate signs the person in with GitHub, registers each MCP host (dynamic client registration) and has the person approve its connection; `/connections` on Gate revokes one. The gateway's MCP URL is a resource registered in Gate, so the gateway lives under Gate's cookie parent (`reach-api.<parent>`). It checks every request's token with Gate (introspection, nothing cached) and admits only the configured person, `OWNER_SUBJECT`.
- **What they may do**: every remote request acts as the configured `client`, and the daemon authorizes it with that client's permissions, as for the local API. Give that client only what remote AIs should do.
- **Connection**: each daemon authenticates with its node's token, pings every 30 s and reconnects with backoff. A new connection of a node replaces its old one; nodes do not affect each other.
- **Failures**: `node_offline` when the node's daemon is not connected (nothing ran). `result_unknown` when the connection dropped or the node did not answer within 30 s: the request may have run, so check it with `get_operation` or resend it with the same `operation_id`. Long work is a session or job the caller polls; a disconnect never stops it.
- The gateway stores nothing but the Durable Objects' connections. Workspaces, sessions and events stay on Linux, on their node.

Deploying the gateway and connecting the daemon are part of [INSTALL.md](INSTALL.md). For development, `npm install` and `npm run check` in `cloud/`; `REACH_E2E_GATEWAY=1 go test ./cmd/reach -run TestRemoteE2E` runs the gateway in workerd (`wrangler dev`) against a fake Gate, with a node on a fake Orca, a node with local workspaces and a node that never connects, and drives the whole remote path as an MCP client.

## Documents

- [INSTALL.md](INSTALL.md) — prompt for a coding agent that installs or updates Reach on a host
- [PRODUCT.md](PRODUCT.md) — product definition, user value, MVP
- [ARCHITECTURE.md](ARCHITECTURE.md) — technical overview, boundaries, execution/connection/recovery contracts
- [docs/davinci-execution-integration.md](docs/davinci-execution-integration.md) — separation of responsibilities between DaVinci and Reach, references to the existing implementation, staged migration and verification
