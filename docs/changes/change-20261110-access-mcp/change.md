---
id: change-20261110-access-mcp
kind: change
title: Replace Gate authentication with direct Cloudflare Access MCP authentication
status: active
created: '2026-11-10'
profile: sdd@1
intent: Replace Gate introspection with validated Access assertions while preserving
  node authorization.
outcomes:
- Validated Access assertions replace Gate introspection without changing node credentials
  or permissions.
- Claude and ChatGPT connect directly through Managed OAuth before migration is accepted.
scope:
- cloud/
- src/cmd/reach/remote_e2e_test.go
- README.md
- ARCHITECTURE.md
- INSTALL.md
- docs/
- .gitignore
non_goals:
- MCP Events, daemon permission changes, consumer migrations and Gate retirement.
change_classes:
- boundary
governance:
  gate: auto
  reasons: []
members:
- role: requirements
  path: changes/change-20261110-access-mcp/requirements.md
  required: true
- role: implementation
  path: changes/change-20261110-access-mcp/implementation.md
  required: true
- role: verification
  path: changes/change-20261110-access-mcp/verification.md
  required: true
promotion: []
unresolved_decisions: []
tags: []
owners: []
relations: []
source_paths: []
summary: Direct Managed OAuth for MCP, isolated from node WebSockets; hosted acceptance
  remains pending.
updated: '2026-11-10'
---

## Summary

Replace Gate introspection with Cloudflare Access assertion verification for
direct MCP access. Preserve the node registry, WebSocket authentication and
Linux permissions.

## Closure Notes

Keep this change active until deployment and both target clients pass hosted
Managed OAuth acceptance. Local code and test completion do not close it.
