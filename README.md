# Syntoria Reach

**Give your AI the ability to act in your environment.**

Syntoria Reach is an independent MCP service that lets AIs such as ChatGPT, Claude, and Codex safely use the files, commands, and jobs in a user's Linux environment.

The AI decides and plans; Reach handles authentication, permission checks, execution, and result retrieval. Reach itself does not need to embed an LLM.

**DaVinci is the first full-scale service built on Reach.** DaVinci owns development planning, agent sessions, issues/PRs, and verification and acceptance of results, while Reach runs and manages Linux processes, asynchronous jobs, and agent sessions over Codex App Server / Claude Code stream-json / ACP. Reach can be used on its own; DaVinci does not have to be running.

## What users can do

- Access permitted workspaces on their own Linux PC or development server from an AI while away from it
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
    L["Linux Reach: Go Connector + Runtime"]
    DB[("Linux SQLite: operations / jobs / audit")]
    OS["Workspace / files / commands / jobs / agent harnesses"]
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

Linux, one user, one node: file operations, permitted command execution, asynchronous jobs, **agent sessions**, permission management, operation history, and remote MCP connections. A local end-to-end run in which DaVinci uses Reach as its execution runtime is part of the initial acceptance criteria. Browser/GUI operation, multiple nodes, managing local agent launches, and WebRTC P2P are candidates for later extensions.

Only **Stages 1 (Local Core) and 2 (Linux Execution) of ARCHITECTURE.md §11 are implemented** so far. Everything else described here is design and does not indicate implemented functionality.

## Running Reach (Stages 1–2)

A single Go daemon, `reach`. It serves the local API as HTTP/JSON over a Unix socket and stores operations, workspaces, jobs, agent sessions, and audit records in SQLite. Job output and harness logs are kept next to the database.

Workspaces live on the [Orca](https://www.onorca.dev/) server of the same host. A caller creates a workspace from a project (a repository registered in Orca); Reach creates it as an Orca worktree through the Orca CLI, and only that caller can use and delete it. Callers never talk to Orca or to the agent protocols themselves: they use Reach's methods, and Reach drives Orca and the agents.

The implementation lives in `src/` (Go module `github.com/takezoh/syntoria-reach/src`).

~~~sh
cd src && go build -o ~/.local/bin/reach ./cmd/reach
chmod 600 ~/.config/reach/config.json          # contains tokens; must not be readable by group/other
reach -config ~/.config/reach/config.json      # systemd user unit: deploy/systemd/reach.service
~~~

~~~json
{
  "orca": ["/home/me/.local/bin/orca-ide"],
  "agents": [
    {"id": "codex", "protocol": "codex", "command": ["/home/me/.local/bin/codex", "app-server"], "sandbox": "workspace-write"},
    {"id": "claude", "protocol": "claude", "command": ["/home/me/.local/bin/claude", "--permission-mode", "acceptEdits"],
     "env": {"DISABLE_AUTOUPDATER": "1"}},
    {"id": "dsh", "protocol": "acp", "command": ["/path/to/acp-agent"], "allow_permissions": false}
  ],
  "projects": [{"id": "app", "repo": "/home/me/src/app",
                "commands": ["/usr/bin/make", "/usr/bin/go"], "agents": ["codex", "claude", "dsh"]}],
  "clients": [{"id": "davinci", "token": "<random string of at least 32 characters>",
               "grants": [{"project": "app",
                           "allow": ["workspace.create", "file.read", "file.write", "command.run", "agent.run"]}]}]
}
~~~

`socket` defaults to `$XDG_RUNTIME_DIR/reach/reach.sock` and `database` defaults to `$XDG_STATE_HOME/reach/reach.db`. `repo` must already be registered in Orca (`orca-ide repo add`). Grants apply to the workspaces the client creates from the project; `workspace.create` allows creating and deleting them.

Requests are JSON sent to `POST /v1/<method>` with `Authorization: Bearer <token>`. The caller must run as the same UID as the daemon (checked with SO_PEERCRED).

| method | Description |
| --- | --- |
| `list_projects` | Projects you may create workspaces from, with your permissions and their commands and agents |
| `create_workspace` | `operation_id`, `project`, `name`. Creates an Orca worktree (and branch) named `name` from the project; returns the `workspace` ID and path |
| `list_workspaces` | Your workspaces |
| `delete_workspace` | `operation_id`, `workspace`. Removes the worktree from Orca and Git and deletes its branch if Orca can prove it is merged. Refused while jobs or agent sessions run, or (by Orca) with uncommitted changes |
| `list_files` | Directory listing for `workspace`, `path` |
| `read_file` | `workspace`, `path`, `offset`, `limit`. Returns `data` (base64) and `version` |
| `write_file` | `operation_id`, `workspace`, `path`, `content` (base64), `expected_version` (empty means create only). Replaces the file by atomic rename |
| `run_command` | `operation_id`, `workspace`, `executable` (absolute path from the allowlist), `args`, `timeout_seconds`. Runs synchronously in the workspace root without a shell |
| `get_operation` | Looks up an operation's state and result by `operation_id` |
| `start_job` | `operation_id`, `workspace`, `executable`, `args`, `timeout_seconds` (default 1 h). Starts the command in the background; the result is the job |
| `get_job` | `job_id`. State (`running`, `succeeded`, `failed`, `cancelled`, `unknown`), exit code |
| `read_job_output` | `job_id`, `stream` (`stdout`/`stderr`), `offset`, `limit`. `eof` is true once the job ended and the end was read |
| `cancel_job` | `operation_id`, `job_id`. Kills the job's process group |
| `start_agent_session` | `operation_id`, `workspace`, `agent`, optional `resume_session_id` (one of your ended sessions in the same workspace and agent; its conversation continues), `model`, `effort` (Codex, Claude). Starts the harness and opens a session |
| `send_agent_prompt` | `operation_id`, `session_id`, `text`. Starts one turn and returns its number without waiting |
| `read_agent_events` | `session_id`, `after` (sequence), `limit`. Events are `message`, `error`, `turn_completed` |
| `get_agent_session` | `session_id`. State (`idle`, `running`, `closed`, `failed`, `unknown`), turns, protocol session ID |
| `stop_agent_session` | `operation_id`, `session_id`. Interrupts any turn and ends the harness |

- Requests with side effects are recorded as operations. Resending the same `operation_id` with the same arguments returns the existing result; different arguments return `operation_conflict`.
- Commands, jobs, and agent harnesses are children of the daemon and die with it. Operations, jobs, and sessions that were running when the daemon stopped become `unknown` at the next start. They are never re-executed or resumed automatically.
- Sandboxing: agents rely on their harness. Codex runs with its `sandbox` mode (default `workspace-write`) and approvals disabled; Claude Code runs with the permission mode and sandbox settings of its configured command and settings (Reach appends only the stream-json and session flags); ACP agents get no file system or terminal capabilities from Reach, and permission requests are denied unless `allow_permissions` is set. Reach does not sandbox commands and jobs: they run as the daemon's user, restricted to the allowlist and a minimal environment.
- Orca creates new workspaces from the repository's default base (e.g. `origin/main`); unpushed local commits are not included.
- Remote MCP (Cloudflare) is not implemented yet.

## Documents

- [PRODUCT.md](PRODUCT.md) — product definition, user value, MVP
- [ARCHITECTURE.md](ARCHITECTURE.md) — technical overview, boundaries, execution/connection/recovery contracts
- [docs/davinci-execution-integration.md](docs/davinci-execution-integration.md) — separation of responsibilities between DaVinci and Reach, references to the existing implementation, staged migration and verification
