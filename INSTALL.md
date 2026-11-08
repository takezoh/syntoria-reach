# Install Reach

This file is a prompt for a coding agent. Run the agent as the user who will own Reach, on the Linux host where Reach should run, from a checkout of this repository, and tell it: "Follow INSTALL.md".

---

You are installing (or updating) the Reach daemon on this host as a systemd user service, from this checkout. README.md ("Running Reach") describes the configuration file and the local API; read it before you start.

## Done when

- `~/.local/bin/reach` is built from this checkout.
- `~/.config/reach/config.json` exists with mode 600, and the user has approved its agents, commands, clients and their permissions.
- `reach.service` is installed in `~/.config/systemd/user/`, enabled, and active.
- `list_capabilities` answers through the socket for every configured client.
- If the user wants remote MCP (section 7): the gateway is deployed and registered in Gate, the daemon is connected to it, and its MCP endpoint refuses requests without a token.
- The user has seen your report (see the last section).

The unit (`deploy/systemd/reach.service`) runs `%h/.local/bin/reach -config %h/.config/reach/config.json`, so use exactly these two paths.

## Rules

- Do not run `sudo`. When something needs root, give the user the command and wait.
- Never show a token or key in the conversation, in command output, or in a file other than the config and the gateway secrets file (section 7). Generate each one inside the command that writes it into its file, and read it from that file into a shell variable when you need it.
- Do not overwrite an existing config. If it needs a change, show the change with tokens redacted, apply it only after the user agrees, and keep the existing tokens.
- Ask the user before you:
  - restart a running `reach.service`. A restart kills its running jobs and agent sessions, and they become `unknown`.
  - run the smoke test. It creates a workspace (a folder registered in Orca as a project), and it runs an agent turn, which costs tokens.
- Do not change this repository.
- If you run in a sandbox, it may block Unix sockets, the systemd user bus, or the Orca CLI. These commands then fail with errors like `EPERM`, "Operation not permitted", or "Failed to connect to bus". Such a failure says nothing about Reach. Run the command outside the sandbox if you are allowed to, or ask the user to run it and paste the output.

## 1. Check the host

Collect every missing prerequisite, then report them all at once. Stop if any of them blocks the install.

- **systemd user manager.** `systemctl --user show-environment` succeeds.
- **Linger.** `loginctl show-user "$USER" -p Linger` prints `Linger=yes`. Without linger, the service stops when the user's last session ends. The user can enable it with `sudo loginctl enable-linger "$USER"`.
- **Go.** `go version` is at least the `go` line in `src/go.mod`.
- **Orca.** The Orca server of this host runs as the same user, and its CLI answers.
  - Find the CLI's absolute path with `command -v orca-ide` or `command -v orca`.
  - `<orca> status --json` shows that the runtime is reachable.
  - `<orca> repo list --json` lists the registered projects.
- **Agents.** Find the absolute path of each agent CLI the user may want: `codex` (protocol `codex`), `claude` (protocol `claude`), and any ACP agent (protocol `acp`).
- **Existing install.** Check whether `~/.local/bin/reach`, `~/.config/reach/config.json` and `~/.config/systemd/user/reach.service` exist, and whether `systemctl --user is-active reach.service` reports active. If any of them exists, this is an update: keep the config, and skip what is already right.

## 2. Agree on the configuration

Propose a configuration from what you found, then let the user decide:

- **Agents.** Which agents may be started; every agent may be started in every workspace.
- **Commands.** Which executables `run_command` and `start_job` may run, as absolute paths.
- **Clients.** The callers: one client per caller, for example `davinci`, or `local` for manual use. Each client has an `id`, a token, and in `allow` its permissions from `workspace.create`, `file.read`, `file.write`, `command.run` and `agent.run`, which apply to every workspace.
- **Socket, database and workspaces.** Leave `socket`, `database` and `workspaces` unset unless the user asks; they then default to `$XDG_RUNTIME_DIR/reach/reach.sock`, `$XDG_STATE_HOME/reach/reach.db` (`~/.local/state/reach/reach.db` when `XDG_STATE_HOME` is unset in the systemd user manager) and `workspaces` next to the database. Reach creates each workspace's folder in `workspaces` and registers it in Orca; job output and harness logs go next to the database; the paths below assume the default.

The configuration names no repositories or Orca projects: a workspace is an Orca project Reach creates, named `reach-*`.

**Child environment.** Commands, jobs and agents do not inherit the daemon's environment. They get exactly `PATH=/usr/local/bin:/usr/bin:/bin`, `LANG=C.UTF-8` and `HOME`, plus the agent's `env`. (Only the Orca CLI gets the daemon's environment.)

- **Agent CLIs.** Check that each agent CLI starts with that environment:

  ```sh
  env -i PATH=/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 HOME="$HOME" <agent> --version
  ```

  Use another harmless flag if the agent has no `--version`. If it does not start (for example, a Node script whose `node` is elsewhere), set `PATH` in that agent's `env` to explicit directories. Do the same when the agent's own tools must find toolchains that are not in `/usr/local/bin:/usr/bin:/bin`. A `PATH` in `env` replaces the fixed one.
- **Commands.** Commands cannot get an extra `PATH`. List the real executables, and tell the user that the scripts they start see only the fixed `PATH`.

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

Any process other than `reach` is a running job or agent session. If there is one, ask the user before running `systemctl --user restart reach.service`.

**Check.** `systemctl --user is-active reach.service` prints `active`, and `journalctl --user -u reach.service -n 20 --no-pager` shows `reach listening`.

**If it fails.** If the service does not start, read the journal. Configuration errors name the field. Fix the cause, then restart.

## 6. Verify

For each client, read its token from the config without printing it, then call `list_capabilities`:

```sh
TOKEN=$(jq -r '.clients[] | select(.id == "<id>") | .token' ~/.config/reach/config.json)
curl -sS --unix-socket "$XDG_RUNTIME_DIR/reach/reach.sock" -X POST \
  -H "Authorization: Bearer $TOKEN" http://reach/v1/list_capabilities
```

The answer is the client's `permissions`, with the `commands` and `agents` it may run. If you get something else:

- `401` means the token does not match the running config.
- "Connection refused" or no socket means the service is not listening, or a sandbox blocks the socket.

**Smoke test (optional).** Only with the user's consent, and with a client that has `workspace.create` and `agent.run`. Use unique `operation_id`s, and a name such as `reach-smoke-<yyyymmdd>` (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`).

1. `create_workspace` with `{"operation_id", "name", "note"}`, the note saying it is the smoke test. The operation's `state` is `succeeded`, and its `result.workspace` is the workspace ID; Orca now lists the project `reach-<name>`. A failure names Orca's error in `error_message`.
2. `start_agent_session` with `{"operation_id", "workspace", "agent"}`. The `result.session_id` is the session. If the harness does not start, the operation fails with `harness_failed`. Its stderr is the newest file in `~/.local/state/reach/agents/`.
3. `send_agent_prompt` with `{"operation_id", "session_id", "text": "Reply with OK and nothing else."}`.
4. Call `read_agent_events` with `{"session_id", "after": 0}` every few seconds until an event of type `turn_completed` arrives; its `text` is the turn status. Show the user the `message` events.
5. `stop_agent_session`, then `update_workspace` with `{"operation_id", "workspace", "status": "completed"}`. The daemon removes the workspace, Orca project and folder, after 7 days without activity.

## 7. Remote MCP (optional)

Ask the user whether remote MCP clients (claude.ai, ChatGPT, Claude Code elsewhere) should reach this node. If so, deploy the gateway in `cloud/` to their Cloudflare account, register it as a resource in their Syntoria Gate, and connect the daemon to it. README.md ("Remote MCP") describes what it does.

1. **Prerequisites.**
   - The user runs Syntoria Gate, or can ask its operator to register a resource. You need three of Gate's settings, which are the Gate service's environment variables:
     - the issuer, `GATE_ISSUER`, for example `https://gate.<parent>`
     - the cookie parent, `GATE_SESSION_COOKIE_DOMAIN`, the `<parent>` below
     - the GitHub account ID Gate admits, `GATE_GITHUB_USER_ID`. The person's Gate subject is `github:<that ID>`.
   - The gateway's host is `reach-api.<parent>`: Gate accepts only resources under its cookie parent, and the other Syntoria APIs are `<app>-api.<parent>`. The `<parent>` zone must be in the user's Cloudflare account. Agree on the host name with the user.
   - `node --version` must be at least 22.
   - Run `npm ci` in `cloud/`.
   - The user signs in to Cloudflare: in `cloud/`, they run `npx wrangler login`, which opens a browser. Alternatively, they set `CLOUDFLARE_API_TOKEN`. Do not handle their Cloudflare credentials yourself.
   - `npx wrangler whoami` shows the account.
2. **Agree on the remote client.** Every remote request acts as one client of the config, with that client's permissions. Propose a dedicated client, for example `remote`, with only what remote AIs should do. For agent work that is `workspace.create` and `agent.run`. Add it as in section 4; its token also works on the local socket.
3. **Secrets.** Write `~/.config/reach/gateway-secrets.env` with mode 600. Every line is uploaded as a Worker secret, which keeps deployment values out of the repository:
   - `GATE_ISSUER`: Gate's issuer.
   - `GATE_CLIENT_ID`: the gateway's resource client ID in Gate, for example `reach-api`.
   - `GATE_CLIENT_SECRET`: its client secret, 64 random hex characters (`openssl rand -hex 32`).
   - `OWNER_SUBJECT`: `github:<ID>`, the only person the gateway admits.
   - `NODE_TOKEN`: the token the daemon connects with, 64 random hex characters.
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
6. **Connect the daemon.**
   - Add `"remote": {"url": "wss://reach-api.<parent>/connect", "token": "<NODE_TOKEN>", "client": "<remote client>"}` to the config, keeping mode 600. Read the token from the secrets file inside the command that writes it.
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
   - `list_capabilities` from that client shows the remote client's permissions.

**Cutting off access.**
- One connection: revoke it on Gate's `/connections` page. It stops working at its next request.
- All remote access: remove `remote` from the config and restart the service. Alternatively, rotate `NODE_TOKEN`: change it in the secrets file and the config, run `npx wrangler secret put NODE_TOKEN` in `cloud/`, then restart the service.

## Report

Tell the user:

- The commit installed, and whether the tree was clean.
- The paths: binary, config, unit, socket, and state directory (database, job output, harness logs).
- The service state.
- Which checks and tests passed, which failed, and which you could not run, and why.
- For each client: its ID, and that its token is in the config at `.clients[] | select(.id == "<id>") | .token`. Do not include the token.
- With remote MCP:
  - the gateway URL and its MCP endpoint (`https://<host>/mcp`)
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

Workspaces stay: the Orca projects named `reach-*` and their folders under `workspaces` (by default `~/.local/state/reach/workspaces`). If the user wants them gone, remove those projects in Orca (`<orca> project setup-delete --setup <repo id> --force`) and then their folders. Remove the config (tokens) and the state directory only if the user asks.

To remove the gateway, run `npx wrangler delete` in `cloud/`. It deletes the Worker and its Durable Object. Then ask the user or Gate's operator to remove the gateway's entry from `GATE_RESOURCES`.
