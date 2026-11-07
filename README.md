# Syntoria Reach

**Give your AI the ability to act in your environment.**

Syntoria Reach is an independent MCP service that lets AIs such as ChatGPT, Claude, and Codex safely use the files, commands, and jobs in a user's Linux environment.

The AI decides and plans; Reach handles authentication, permission checks, execution, and result retrieval. Reach itself does not need to embed an LLM.

**DaVinci is the first full-scale service built on Reach.** DaVinci owns development planning, agent sessions, issues/PRs, and verification and acceptance of results, while Reach runs and manages Linux processes, sandboxes, interactive I/O, and asynchronous jobs. Reach can be used on its own; DaVinci does not have to be running.

## What users can do

- Access permitted workspaces on their own Linux PC or development server from an AI while away from it
- Read and write files, and run commands, builds, and tests
- Start long-running work as a job and check its progress and results later, even if the connection to the AI drops
- Manage interactive processes that need continuous stdin/stdout from client services such as DaVinci
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
    OS["Workspace / files / sandboxed commands / interactive processes"]
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

Linux, one user, one node: file operations, permitted command execution, asynchronous jobs, **interactive process sessions**, permission management, operation history, and remote MCP connections. A local end-to-end run in which DaVinci uses Reach as its execution runtime is part of the initial acceptance criteria. Browser/GUI operation, multiple nodes, managing local agent launches, and WebRTC P2P are candidates for later extensions.

Only **Stage 1 (Local Core) of ARCHITECTURE.md §11 is implemented** so far. Everything else described here is design and does not indicate implemented functionality.

## Stage 1: Local Core

A single Go daemon, `reach`. It serves the local API as HTTP/JSON over a Unix socket and stores operations and audit records in SQLite.

The implementation lives in `src/` (Go module `github.com/takezoh/syntoria-reach/src`).

~~~sh
cd src && go build -o ~/.local/bin/reach ./cmd/reach
chmod 600 ~/.config/reach/config.json          # contains tokens; must not be readable by group/other
reach -config ~/.config/reach/config.json      # systemd user unit: deploy/systemd/reach.service
~~~

~~~json
{
  "workspaces": [{"id": "app", "root": "/home/me/src/app", "commands": ["/usr/bin/make", "/usr/bin/go"]}],
  "clients": [{"id": "davinci", "token": "<random string of at least 32 characters>",
               "grants": [{"workspace": "app", "allow": ["file.read", "file.write", "command.run"]}]}]
}
~~~

`socket` defaults to `$XDG_RUNTIME_DIR/reach/reach.sock` and `database` defaults to `$XDG_STATE_HOME/reach/reach.db`.

Requests are JSON sent to `POST /v1/<method>` with `Authorization: Bearer <token>`. The caller must run as the same UID as the daemon (checked with SO_PEERCRED).

| method | Description |
| --- | --- |
| `list_workspaces` | Permitted workspaces and their permissions |
| `list_files` | Directory listing for `workspace`, `path` |
| `read_file` | `workspace`, `path`, `offset`, `limit`. Returns `data` (base64) and `version` |
| `write_file` | `operation_id`, `workspace`, `path`, `content` (base64), `expected_version` (empty means create only). Replaces the file by atomic rename |
| `run_command` | `operation_id`, `workspace`, `executable` (absolute path from the allowlist), `args`, `timeout_seconds`. Runs synchronously in the workspace root without a shell |
| `get_operation` | Looks up an operation's state and result by `operation_id` |

- Requests with side effects are recorded as operations. Resending the same `operation_id` with the same arguments returns the existing result; different arguments return `operation_conflict`.
- Operations that were running when the daemon stopped become `unknown` at the next start. They are never re-executed automatically.
- Stage 1 limitation: commands run without a sandbox, as the same user as the daemon. The only restrictions are the allowlist and a minimal environment; isolation is added in Stage 2. Asynchronous jobs, process sessions, and remote MCP are not implemented yet.

## Documents

- [PRODUCT.md](PRODUCT.md) — product definition, user value, MVP
- [ARCHITECTURE.md](ARCHITECTURE.md) — technical overview, boundaries, execution/connection/recovery contracts
- [docs/davinci-execution-integration.md](docs/davinci-execution-integration.md) — separation of responsibilities between DaVinci and Reach, references to the existing implementation, staged migration and verification
