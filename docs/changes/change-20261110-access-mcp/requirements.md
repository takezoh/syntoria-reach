---
change: change-20261110-access-mcp
role: requirements
---

# Requirements

## Accepted direction

Use Cloudflare Access Managed OAuth directly, without MCP Server Portal.
Individual AI client identity is removed; human versus AI boundaries remain.
Migrate Reach before Weave and Lore, and keep Gate until consumers have moved.

## Observable behavior

- A valid, unexpired RS256 Access assertion for the configured team, MCP app
  audience and owner permits the existing MCP tools.
- Missing, malformed, incorrectly signed, expired or wrong-audience assertions
  fail with 401. Another authenticated email or a service-token assertion fails with 403.
- An Authorization bearer token or browser cookie alone cannot bypass the
  assertion requirement. Missing configuration and unavailable signing keys
  fail closed with 503.
- Existing node IDs, credentials, brokers and Linux permissions remain intact;
  nodes use their independent WebSocket credentials without interactive OAuth.
- Access owns OAuth discovery at the edge. The Worker does not advertise Gate
  metadata or implement a second OAuth flow.
- Hosted acceptance requires both Claude and ChatGPT connecting directly.
  Local checks do not establish this condition.

## Boundaries

Do not implement MCP Events, change daemon permissions, stop Gate, migrate
other consumers, rotate node secrets or delete probe resources in this change.
Service-token admission for non-interactive programs needs a separate explicit
contract: this implementation admits only the configured owner email and a nonempty user subject.
