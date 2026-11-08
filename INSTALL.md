# Install Reach

This file is a prompt for a coding agent. Run the agent as the user who will own Reach, on the Linux host where Reach should run, from a checkout of this repository, and tell it: "Follow INSTALL.md".

---

You are installing (or updating) the Reach daemon on this host as a systemd user service, from this checkout. README.md ("Running Reach") describes the configuration file and the local API; read it before you start.

## Done when

- `~/.local/bin/reach` is built from this checkout.
- `~/.config/reach/config.json` exists with mode 600, and the user has approved its agents, commands, clients and their permissions.
- `reach.service` is installed in `~/.config/systemd/user/`, enabled, and active.
- `list_capabilities` answers through the socket for every configured client.
- If the user wants remote MCP (section 7): the gateway is deployed and registered in Gate, this host is one of its nodes, the daemon is connected to it, and its MCP endpoint refuses requests without a token.
- The user has seen your report (see the last section).

The unit (`deploy/systemd/reach.service`) runs `%h/.local/bin/reach -config %h/.config/reach/config.json`, so use exactly these two paths.

## Rules

- Do not run `sudo`. When something needs root, give the user the command and wait.
- Never show a token or key in the conversation, in command output, or in a file other than the config and the gateway secrets file (section 7). Generate each one inside the command that writes it into its file, and read it from that file into a shell variable when you need it.
- Do not overwrite an existing config. If it needs a change, show the change with tokens redacted, apply it only after the user agrees, and keep the existing tokens.
- Ask the user before you:
  - restart a running `reach.service`. A restart kills its running jobs and agent sessions, and they become `unknown`.
  - run the smoke test. It creates a workspace (a folder, registered in Orca as a project on an Orca host), and it runs an agent turn, which costs tokens.
- Do not change this repository.
- If you run in a sandbox, it may block Unix sockets, the systemd user bus, or the Orca CLI. These commands then fail with errors like `EPERM`, "Operation not permitted", or "Failed to connect to bus". Such a failure says nothing about Reach. Run the command outside the sandbox if you are allowed to, or ask the user to run it and paste the output.

## 1. Check the host

Collect every missing prerequisite, then report them all at once. Stop if any of them blocks the install.

- **systemd user manager.** `systemctl --user show-environment` succeeds.
- **Linger.** `loginctl show-user "$USER" -p Linger` prints `Linger=yes`. Without linger, the service stops when the user's last session ends. The user can enable it with `sudo loginctl enable-linger "$USER"`.
- **Go.** `go version` is at least the `go` line in `src/go.mod`.
- **Orca (optional).** Whether the Orca server of this host runs as the same user and its CLI answers. It decides the node's workspaces (section 2): with Orca, workspaces are Orca projects and agents run in Orca terminals; without it, workspaces are folders Reach keeps and agents run as processes of the daemon.
  - Find the CLI's absolute path with `command -v orca-ide` or `command -v orca`.
  - `<orca> status --json` shows that the runtime is reachable.
  - `<orca> repo list --json` lists the registered projects.
  - `<orca> agent hooks status --json` shows `enabled: true`, and `state: installed` for each agent the user may want: Reach reads an agent's replies from the status these hooks report to Orca.
- **Agents.** Find the absolute path of each agent CLI the user may want, for example `claude`, `codex`, or an ACP agent such as DSH. On Orca, Reach starts an agent by typing its command into an Orca terminal, so the CLI must be the interactive one Orca recognizes. Without Orca, Reach runs the agent itself and talks to it over a protocol: Claude Code over stream-json, Codex as `codex app-server`, or the Agent Client Protocol.
- **Existing install.** Check whether `~/.local/bin/reach`, `~/.config/reach/config.json` and `~/.config/systemd/user/reach.service` exist, and whether `systemctl --user is-active reach.service` reports active. If any of them exists, this is an update: keep the config, and skip what is already right.
- **Existing gateway.** Ask the user whether they already run a Reach gateway that other hosts connect to. If so, this host becomes one more node of it (section 7), and the gateway is not deployed again from scratch.

## 2. Agree on the configuration

Propose a configuration from what you found, then let the user decide:

- **Workspaces.** With Orca, set `orca` to the CLI command, for example `["<orca>"]`. Without Orca, leave `orca` out: the node then keeps its workspaces as local folders and runs agents itself.
- **Agents.** Which agents may be started, each with an `id`, a `role` and a `command`. Every agent may be started in every workspace. Model, effort, permission and sandbox flags belong in the command.
  - `role` says what the agent is for, in a sentence, for example "Develops and reviews code in a repository" or "Turns GitHub issues into draft pull requests with DaVinci". Remote AIs choose a node and an agent by it (`list_nodes`), so make each role tell the agents apart. The daemon refuses an agent without one; on an update, agree on a role for each existing agent.
  - On Orca, `command` is typed into a terminal, for example `["<claude>", "--model", "claude-opus-5-5", "--effort", "xhigh"]`. On an update, propose removing `--ax-screen-reader` from an agent's command: Reach no longer reads the terminal's output, and the flag only replaces the view the user reads in Orca with plain lines. `protocol` and `env` are refused.
  - Without Orca, each agent also has a `protocol`: `claude` (Reach adds the stream-json flags and a session ID to the command), `codex` (the command runs `codex app-server`; Reach turns approvals off, so set the sandbox in the command, for example `-c sandbox_mode="workspace-write"`) or `acp` (Reach cancels permission requests, so the agent's own policy must allow what it needs). No one answers an agent's permission questions there: what the agent may do without asking must be in its settings or command (for Claude Code, its permission settings or `--permission-mode`), and the user decides it. `env` holds environment variables the agent needs beyond the fixed ones below, such as the location of its credentials.
- **Commands.** Which executables `run_command` and `start_job` may run, as absolute paths.
- **Clients.** The callers: one client per caller, for example `davinci`, or `local` for manual use. Each client has an `id`, a token, and in `allow` its permissions from `workspace.create`, `file.read`, `file.write`, `command.run` and `agent.run`, which apply to every workspace.
- **Socket, database and workspaces.** Leave `socket`, `database` and `workspaces` unset unless the user asks; they then default to `$XDG_RUNTIME_DIR/reach/reach.sock`, `$XDG_STATE_HOME/reach/reach.db` (`~/.local/state/reach/reach.db` when `XDG_STATE_HOME` is unset in the systemd user manager) and `workspaces` next to the database. Reach creates each workspace's folder in `workspaces` (and on Orca registers it as a project); job output and the logs of local agents go next to the database; the paths below assume the default.

The configuration names no repositories or Orca projects: a workspace is a folder Reach creates, named `reach-*` (on Orca, the project of that name).

**Child environment.** Commands and jobs do not inherit the daemon's environment. They get exactly `PATH=/usr/local/bin:/usr/bin:/bin`, `LANG=C.UTF-8` and `HOME`. Commands cannot get an extra `PATH`: list the real executables, and tell the user that the scripts they start see only the fixed `PATH`. Agents without Orca get the same, plus their `env`; an agent CLI that needs more on its `PATH` gets it there. (The Orca CLI gets the daemon's environment.)

**Agents in Orca.** An agent runs in an Orca terminal of the workspace, in the user's login shell, so it gets the user's own environment. Claude Code must start there without a question to answer:

- In `~/.claude.json`, `projects."<workspaces folder>".hasTrustDialogAccepted` is `true`. Claude Code applies a parent folder's trust to the folders inside it, so this covers every workspace.
- In `~/.claude.json`, `autoModeEnvSetup.dismissed` is `true`, so the auto mode setup prompt does not appear after the first turn.

The `agent-claude` module of Agent Environments sets both. If the host does not use it, tell the user and ask before you change `~/.claude.json`; do it while no Claude Code runs, since it rewrites that file. Without Orca, Claude Code runs with `--print`, which shows no trust prompt.

Show the user the complete config, with tokens redacted, and wait for their approval.

## 3. Build

In `src/`:

```sh
go vet ./...
go test ./...
go build -o ~/.local/bin/reach ./cmd/reach
```

The tests of `internal/localapi` and `cmd/reach` use Unix sockets; see the sandbox rule. Do not install a build whose tests fail. Record `git rev-parse HEAD`, and record whether `git status --porcelain` is empty.

## 4. Write the config

- Create `~/.config/reach/` with mode 700.
- Write `config.json` with mode 600 from the start: use `umask 077`, or write to a temporary file in the same directory and rename it.
- Make each token a random value of at least 32 characters, for example from `openssl rand -hex 32`. Generate it inside the command that writes the file, for example a `jq --arg` that sets `.clients[] | select(.id == "<id>") | .token`.
- Check the result with `stat -c %a ~/.config/reach/config.json` (`600`), and parse it with `jq empty`.

The daemon refuses to start if group or others have any access to the file, or if the configuration is invalid.

## 5. Install and start the service

```sh
install -D -m 644 deploy/systemd/reach.service ~/.config/systemd/user/reach.service
systemctl --user daemon-reload
systemctl --user enable --now reach.service
```

**Update.** For an update, restart instead. First look at the processes in the service's cgroup:

```sh
systemctl --user status reach.service
```

Any process other than `reach` is a running job, or, without Orca, a running agent session. If there is one, ask the user before running `systemctl --user restart reach.service`.

**Check.** `systemctl --user is-active reach.service` prints `active`, and `journalctl --user -u reach.service -n 20 --no-pager` shows `reach listening`, with `workspaces=orca` or `workspaces=local`.

**If it fails.** If the service does not start, read the journal. Configuration errors name the field. Fix the cause, then restart.

## 6. Verify

For each client, read its token from the config without printing it, then call `list_capabilities`:

```sh
TOKEN=$(jq -r '.clients[] | select(.id == "<id>") | .token' ~/.config/reach/config.json)
curl -sS --unix-socket "$XDG_RUNTIME_DIR/reach/reach.sock" -X POST \
  -H "Authorization: Bearer $TOKEN" http://reach/v1/list_capabilities
```

The answer is the client's `permissions`, with the `commands` and the `agents` (each with its `id` and `role`) it may run. If you get something else:

- `401` means the token does not match the running config.
- "Connection refused" or no socket means the service is not listening, or a sandbox blocks the socket.

**Smoke test (optional).** Only with the user's consent, and with a client that has `workspace.create` and `agent.run`. Use unique `operation_id`s, and a name such as `reach-smoke-<yyyymmdd>` (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`).

1. `create_workspace` with `{"operation_id", "name", "note"}`, the note saying it is the smoke test. The operation's `state` is `succeeded`, and its `result.workspace` is the workspace ID; the folder `reach-<name>` exists under `workspaces`, and on Orca, Orca lists the project `reach-<name>`. A failure names Orca's error in `error_message`.
2. `start_agent_session` with `{"operation_id", "workspace", "agent"}`. The `result.session_id` is the session and its `result.state` is `idle` (`working` while the agent still starts). On Orca, Orca shows the agent in a terminal of the workspace; if `state` is `waiting_on_human`, the agent asks something at its start, such as a trust prompt: look at its terminal in Orca. Without Orca, `harness_unavailable` means the agent did not start or did not speak its protocol: its stderr is in `~/.local/state/reach/agents/<session_id>.stderr`.
3. `send_agent_prompt` with `{"operation_id", "session_id", "text": "Reply with OK and nothing else."}`. If it fails with `harness_unavailable`, the agent did not start a turn: look at its terminal in Orca.
4. Call `read_agent_output` with `{"session_id"}` until `state` is `idle`. Show the user the `reply`: it is `OK`. On Orca, if it stays empty, Orca's agent status hooks are not reporting the agent: check `<orca> agent hooks status --json`. Without Orca, a failed turn leaves its error as the reply.
5. `stop_agent_session`, then `update_workspace` with `{"operation_id", "workspace", "status": "completed"}`. The daemon removes the workspace (on Orca, the project) and its folder after 7 days without activity.

## 7. Remote MCP (optional)

Ask the user whether remote MCP clients (claude.ai, ChatGPT, Claude Code elsewhere) should reach this host. If so, this host becomes a node of the user's Reach gateway: deploy the gateway in `cloud/` to their Cloudflare account and register it as a resource in their Syntoria Gate (unless it exists already), register the node on it, and connect the daemon to it. README.md ("Remote MCP") describes what it does.

One gateway serves all of the user's nodes. If the gateway exists already (section 1), skip step 4 and the Gate secrets of step 3: agree on the node ID and the remote client, add the node to `NODES` (step 3), upload it (step 5), and connect the daemon (step 6).

1. **Prerequisites.**
   - The user runs Syntoria Gate, or can ask its operator to register a resource. You need three of Gate's settings, which are the Gate service's environment variables:
     - the issuer, `GATE_ISSUER`, for example `https://gate.<parent>`
     - the cookie parent, `GATE_SESSION_COOKIE_DOMAIN`, the `<parent>` below
     - the GitHub account ID Gate admits, `GATE_GITHUB_USER_ID`. The person's Gate subject is `github:<that ID>`.
   - The gateway's host is `reach-api.<parent>`: Gate accepts only resources under its cookie parent, and the other Syntoria APIs are `<app>-api.<parent>`. The `<parent>` zone must be in the user's Cloudflare account. Agree on the host name with the user.
   - `node --version` must be at least 22.
   - **The node ID.** Agree with the user on a short ID for this host, such as `desktop` or `build-server` (letters, digits and `._-`, at most 64). Remote AIs see it in `list_nodes` and name it in every call.
   - Run `npm ci` in `cloud/`.
   - The user signs in to Cloudflare: in `cloud/`, they run `npx wrangler login`, which opens a browser. Alternatively, they set `CLOUDFLARE_API_TOKEN`. Do not handle their Cloudflare credentials yourself.
   - `npx wrangler whoami` shows the account.
2. **Agree on the remote client.** Every remote request acts as one client of the config, with that client's permissions. Propose a dedicated client, for example `remote`, with only what remote AIs should do. For agent work that is `workspace.create` and `agent.run`. Add it as in section 4; its token also works on the local socket.
3. **Secrets.** Write `~/.config/reach/gateway-secrets.env` with mode 600. Every line is uploaded as a Worker secret, which keeps deployment values out of the repository:
   - `GATE_ISSUER`: Gate's issuer.
   - `GATE_CLIENT_ID`: the gateway's resource client ID in Gate, for example `reach-api`.
   - `GATE_CLIENT_SECRET`: its client secret, 64 random hex characters (`openssl rand -hex 32`).
   - `OWNER_SUBJECT`: `github:<ID>`, the only person the gateway admits.
   - `NODES`: the nodes and their tokens, as one line of JSON, `NODES={"<node ID>":"<token>",...}`. Each node has a token of its own, 64 random hex characters, that its daemon connects with. To add this host to an existing gateway, add its entry and keep the others.
   - **Update from one node.** A gateway that predates multiple nodes has `NODE_TOKEN` instead. Replace that line with `NODES={"<node ID>":"<the NODE_TOKEN value>"}`, so that the connected daemon keeps its token, and after the deploy run `npx wrangler secret delete NODE_TOKEN` in `cloud/`. The deploy does not drop the daemon's connection, which stays with the earlier gateway's broker, where no request reaches it any more: restart `reach.service` after the deploy (the restart rule applies), and check that the journal shows `connected to gateway` again. Tell the user that every tool but `list_nodes` now takes a required `node`: MCP clients that listed the tools before the deploy fail with the old arguments until they list them again (reconnect them, or start a new conversation).
4. **Register the gateway in Gate.** Add this entry to Gate's `GATE_RESOURCES` secret:

   ```json
   {"audience": "https://reach-api.<parent>/mcp", "client_id": "<GATE_CLIENT_ID>", "client_secret": "<GATE_CLIENT_SECRET>", "browser_client_id": "reach-web"}
   ```

   - Gate requires a `browser_client_id`. The gateway accepts no browser sessions, so the value is unused, but it must be unique among Gate's resources.
   - Add the payload as a new secret version, then deploy a new Gate revision that reads it. Gate's `docs/deployment.md` describes both.
   - The payload holds the other resources' secrets, so the user or Gate's operator does this step. Do not read the payload yourself.
5. **Deploy.** In `cloud/`, run `npx wrangler deploy --domain reach-api.<parent> --secrets-file ~/.config/reach/gateway-secrets.env`.
   - Pass `--domain` on every deploy. The gateway has no `workers.dev` URL.
   - Never commit the secrets file. It lives outside the repository.
   - To change only the nodes of a deployed gateway, run `npx wrangler secret put NODES` in `cloud/` and give it the new JSON from the secrets file, without printing it. The secrets file can be on another host, the one that deployed the gateway; then the user runs this there.
6. **Connect the daemon.**
   - Add `"remote": {"url": "wss://reach-api.<parent>/connect", "token": "<this node's token>", "client": "<remote client>"}` to the config, keeping mode 600. Read the token from the secrets file (`NODES`) inside the command that writes it. The gateway knows the node by its token.
   - If the installed binary predates this checkout, rebuild it (section 3).
   - Restart `reach.service`; the restart rule applies.
   - `journalctl --user -u reach.service -n 20 --no-pager` then shows `connected to gateway`.
7. **Verify.**
   - `curl -sS https://reach-api.<parent>/` answers with the gateway's name.
   - `curl -sS https://reach-api.<parent>/.well-known/oauth-protected-resource/mcp` names Gate's issuer in `authorization_servers`.
   - `curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://reach-api.<parent>/mcp` prints `401`.
   - Then the user connects an MCP client to `https://reach-api.<parent>/mcp`:
     - claude.ai: Settings → Connectors → Add custom connector.
     - Claude Code: `claude mcp add --transport http reach https://reach-api.<parent>/mcp`, then `/mcp` to sign in.
   - Gate asks the user to sign in with GitHub and to approve the connection.
   - `list_nodes` from that client shows this node online, with its agents and their roles, and `list_capabilities` with `{"node": "<node ID>"}` shows the remote client's permissions.

**Cutting off access.**
- One connection: revoke it on Gate's `/connections` page. It stops working at its next request.
- One node: remove `remote` from its config and restart its service, or remove its entry from `NODES` (or give it a new token) and upload `NODES` (step 5). To rotate its token, change it in `NODES` and in the node's config, upload `NODES`, then restart the node's service.

## Report

Tell the user:

- The commit installed, and whether the tree was clean.
- The paths: binary, config, unit, socket, and state directory (database, job output, workspace folders).
- The service state.
- Which checks and tests passed, which failed, and which you could not run, and why.
- For each client: its ID, and that its token is in the config at `.clients[] | select(.id == "<id>") | .token`. Do not include the token.
- With remote MCP:
  - the gateway URL and its MCP endpoint (`https://<host>/mcp`)
  - this host's node ID, and the other nodes in `NODES` (IDs only)
  - the remote client and its permissions
  - the Gate issuer, and that the user approves and revokes MCP connections at Gate (`/connections`)
  - whether the daemon is connected
- Anything left to do, for example enabling linger, or configuring the caller to use the socket and its token.

## Uninstall

Only when the user asks:

```sh
systemctl --user disable --now reach.service
rm ~/.config/systemd/user/reach.service ~/.local/bin/reach
systemctl --user daemon-reload
```

Workspaces stay: their folders under `workspaces` (by default `~/.local/state/reach/workspaces`), and on Orca the projects named `reach-*`. If the user wants them gone, remove those projects in Orca (`<orca> project setup-delete --setup <repo id> --force`) and then their folders. Remove the config (tokens) and the state directory only if the user asks.

If the host is a node of a gateway, remove its entry from `NODES` and upload it (section 7, step 5). To remove the gateway itself, run `npx wrangler delete` in `cloud/`. It deletes the Worker and its Durable Objects. Then ask the user or Gate's operator to remove the gateway's entry from `GATE_RESOURCES`.
