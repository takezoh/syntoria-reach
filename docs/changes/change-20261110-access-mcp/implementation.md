---
change: change-20261110-access-mcp
role: implementation
---

# Implementation

## Authentication contract

`cloud/src/access.ts` uses the pinned jose package to validate the
`Cf-Access-Jwt-Assertion` JWT with a remote JWKS from the operator-configured
issuer. Only RS256 is accepted. Issuer, audience, subject, expiry and issued-at
claims are required; jose also validates not-before when present. An exact
owner email check and a nonempty user subject check follow cryptographic verification.
The owner email is operator configuration and is restricted by the Access allow
policy as well. No initial subject bootstrap or per-client identity is needed. No AI client identifier
is returned or stored.

The JWKS cache belongs to the current configuration; issuer, audience or owner email
changes replace the verifier. Library defaults bound cache age and unknown-key
refresh frequency. Key lookup failures caused by unavailable or malformed JWKS
are 503. Invalid assertions are 401; a different email or empty subject is 403. Issuer URLs
must be HTTPS origins; HTTP loopback is allowed for isolated local tests only.

`cloud/src/index.ts` keeps `/connect` and node-token authorization independent.
It removes Gate discovery and introspection. Access settings and the existing
`NODES` map are operator inputs uploaded outside tracked files.

## Delivery boundary

Code and local verification are prepared in this checkout. The installation
procedure describes an Access application scoped to `/mcp`, hosted acceptance,
and rollback of the Worker and edge configuration together. No deployment
success is inferred from these instructions.
