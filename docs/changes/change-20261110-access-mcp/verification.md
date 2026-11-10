---
change: change-20261110-access-mcp
role: verification
---

# Verification

## Local evidence

- `npm run check` in `cloud/`: passed (Worker types and TypeScript).
- `go vet ./...` in `src/`: passed.
- `REACH_E2E_GATEWAY=1 go test ./...` in `src/`: passed with the final claim
  and invalid-configuration cases included.
- `REACH_E2E_GATEWAY=1 go test ./cmd/reach -run '^TestRemote' -count=1 -v`:
  both remote tests and all invalid-configuration subtests passed.
- `docs lint --conformance`, documentation language and local-link checks:
  passed.
- `git diff --check`: passed.

The E2E harness runs the real Worker in workerd, authenticates real daemons over
WebSockets, exercises workspaces and agent turns on both Orca and local backends,
and checks node disconnect/reconnect. The fake Access issuer supplies real RSA
signatures and public keys. Tests verify signature, audience, issuer, expiry,
required claims, not-before, owner admission, bearer-only rejection, invalid
configuration, node connectivity without MCP configuration, JWKS outage and
recovery, and reuse of cached signing keys. They do not emulate Managed OAuth
login or Cloudflare policy evaluation.

## Remaining acceptance

Hosted acceptance requires Access application readback, deployment, Claude
and ChatGPT login, OAuth discovery at the edge and policy/expiry observations.
Keep their identifiers, execution state and results in ignored operator records.
Do not close this change as a completed migration based on local checks.
