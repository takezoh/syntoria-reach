---
id: note-20261121-mcp-events-external-references
kind: note
title: MCP Events external references
status: draft
created: '2026-11-21'
tags: []
owners: []
relations: []
source_paths: []
summary: Digest of SEP-3415 and ChatGPT MCP Events for implementers of change-20261119-mcp-events
---

## Summary

A digest of SEP-3415 and OpenAI's ChatGPT MCP Events documentation for implementers of docs/changes/change-20261119-mcp-events, who cannot read the sources directly. The sources stay authoritative.

## Notes

A working digest of the two external specifications the plan
(`architecture-plan.json`) treats as authoritative, written for implementers
who cannot read them directly. It paraphrases the normative parts and keeps
field, method, header and code names exact. Where this digest and the source
disagree, the source wins; read it when in doubt.

- SEP-3415 Events extension, read 2026-11-21:
  https://raw.githubusercontent.com/modelcontextprotocol/modelcontextprotocol/main/docs/seps/3415-events-extension.mdx
  (marked Final; it names the `ext-events` repository as the current text).
- OpenAI, MCP Events for ChatGPT, read 2026-11-21:
  https://developers.openai.com/plugins/build/mcp-events

"Not specified" means the source is silent; the plan or the implementation
chooses.

## SEP-3415 (standard profile, `/mcp`)

### Discovery and listing

- Extension id `io.modelcontextprotocol/events`, defined for protocol revision
  `2026-07-28` and later only. The server advertises it in the `capabilities`
  of `server/discover` under `extensions["io.modelcontextprotocol/events"]`;
  the settings object may hold `listChanged` (default false). An empty object
  declares support without list-change notifications.
- A server that does not advertise it answers `events/*` with `-32601`.
- Every result is a standard Result with `resultType: "complete"`.
- `events/list`: optional `cursor` param; result `events` (descriptors) and
  `nextCursor` when more pages exist. Descriptor fields: `name`,
  `description`, `delivery` (non-empty subset of `"poll"`, `"push"`,
  `"webhook"`), `inputSchema`, `payloadSchema`, optional `_meta`.

### Event occurrence

Fields: `eventId` (stable upstream id; dedupe is per subscription), `name`,
`timestamp` (ISO 8601, when it occurred), `data` (matches `payloadSchema`),
optional `cursor` (push and webhook only; poll has it at the result level),
optional `_meta`.

### `events/poll`

- Params: `name`, `arguments`, `cursor` (string or null), optional `maxAgeMs`,
  optional `maxEvents`. A `null` cursor means start from now and bootstraps
  the subscription; there is no separate subscribe step.
- Result: `events`, `cursor`, `truncated`, `hasMore`, `nextPollMs`. With
  `hasMore: true` the client polls again at once.
- A stale cursor (outside retention) is never an error: reset to a servable
  position, return it, set `truncated: true`. `maxAgeMs` sets a replay floor
  (later of cursor and now minus `maxAgeMs`), ignored for a null cursor.
- Errors are JSON-RPC errors: NotFound, Forbidden, InvalidParams, Unsupported.

### `events/stream` (push)

- Streamable HTTP POST returning SSE. Params: `name`, `arguments`, `cursor`,
  `maxAgeMs`, optional `heartbeatMs`. An invalid subscription gets an
  immediate JSON-RPC error and no stream.
- Frames in order, each a JSON-RPC notification in an SSE `data:` frame, each
  carrying the stream request's id in
  `params._meta["io.modelcontextprotocol/subscriptionId"]`:
  1. `notifications/events/active`: `cursor`, `truncated`, `heartbeatMs`
     (the server's grant is authoritative; recommended default 30 s). Sent
     again mid-stream with `truncated: true` and a fresh cursor on a gap.
  2. `notifications/events/event`: `eventId`, `name`, `timestamp`, `data`,
     `cursor`.
  3. `notifications/events/error` (optional, recoverable): `error` with
     `code`, `message`, `data`.
  4. `notifications/events/heartbeat`: `cursor` (position checked up to).
     The server must send one whenever `heartbeatMs` passes without another
     message; SSE comments are not used.
  5. `notifications/events/terminated`: `error`; the subscription has ended.
  6. Final result on a server-initiated close: the JSON-RPC response to the
     request id, `resultType: "complete"`, empty. Clients must not depend on it.
- Abort (TCP close or HTTP/2 RST_STREAM) sends nothing; the server must stop
  delivering and release resources. Reconnect is a new `events/stream` with
  the last cursor.

### Webhook lifecycle

- `events/subscribe` params: `name`, `arguments`,
  `delivery: { mode: "webhook", url, secret }`, optional `cursor`,
  `maxAgeMs`, `ttlMs` (omitted: server default; `null`: no expiry requested).
- Result: `id` (server-derived, deterministic over the key, stable across
  refreshes; examples use `sub_`; derivation not specified), `refreshBefore`
  (ISO 8601, or null only when `ttlMs: null` was requested and granted),
  `cursor`, `truncated`, optional `deliveryStatus`.
- Key: `(principal, delivery.url, name, arguments)`, arguments compared by
  canonical JSON. Subscribing again with the same key refreshes: replaces the
  secret (dual-sign old and new for a short grace window), treats `cursor` as
  the client's persisted position, grants the TTL again, reactivates.
- Subscribe and unsubscribe require an authenticated principal, else
  Forbidden.
- `events/unsubscribe` params: `name`, `arguments`, `delivery: { url }`;
  empty result; NotFound when no subscription matches. The `id` is never
  accepted as input.
- Secret: required, client-supplied, `whsec_` followed by base64 of 24 to 64
  bytes; anything else is InvalidParams.
- Callback URLs must be `https://`; a non-https URL is InvalidParams.

### Verification challenge

- Before activating delivery to an unverified `(principal, url)`, POST a
  signed control envelope with the Standard Webhooks headers and
  `X-MCP-Subscription-Id`; body `{"type":"verification","challenge":"<nonce>"}`
  with a single-use, short-lived nonce.
- Activation requires a 2xx whose body is `{"challenge":"<nonce>"}`, compared
  in constant time. A reachable endpoint that does not echo it is
  CallbackEndpointError with `data.reason: "challenge_failed"`; an
  unreachable one uses the connection category (`connection_refused`,
  `timeout`, `tls_error`).
- Verification may be cached per `(principal, url)`. It must use the same
  SSRF-hardened path as deliveries and should be rate-limited per host.
- Timeout, nonce length and attempt count: not specified.

### Delivery

- POST with `Content-Type: application/json`, `webhook-id`,
  `webhook-timestamp` (Unix seconds), `webhook-signature`,
  `X-MCP-Subscription-Id`. Body: the occurrence fields `eventId`, `name`,
  `timestamp`, `data`, `cursor` (a safe-to-persist watermark). A body with a
  top-level `type` is a control envelope.
- `webhook-id`: server-generated, high-entropy, unique per message (one event
  on one subscription, or one control envelope), reused across that message's
  retries, only ASCII letters, digits, `_` and `-`, never derived from
  `eventId`. Control envelopes use `msg_<type>_<random>`.
- Signature (Standard Webhooks): HMAC-SHA256 keyed by the base64-decoded bytes
  after `whsec_`, over `webhook-id + "." + webhook-timestamp + "." + body`
  (raw bytes); header value `v1,` plus base64 of the MAC, several
  space-separated during rotation. Each retry regenerates timestamp and
  signature and keeps `webhook-id`.
- Control envelopes: `{"type":"gap","cursor":"<fresh>"}` and
  `{"type":"terminated","error":{"code","message","data"}}`, both signed with
  the same headers.
- Retries: per event, exponential backoff on non-2xx; the source suggests
  bounded attempts (example 3 to 5) over at most 10 to 15 minutes and never
  indefinitely (the plan fixes at most ten attempts within the grant).
  Terminal: 2xx accepted, `410` (that delivery only), `413` (that event).
  Retryable: `503` and `425`. A timeout around 5 seconds is common.
- `deliveryStatus`: `active` (false means suspended after repeated failures;
  a refresh reactivates), `lastError` (one of `connection_refused`,
  `timeout`, `tls_error`, `http_4xx`, `http_5xx`, `challenge_failed`, or
  null; never raw bodies, headers or status lines), optional `throttled` and
  `retryAfterMs`.

### Destination security

- Validate callback URLs at delivery time, not only at subscribe time; reject
  resolved addresses that are not globally routable per the IANA
  special-purpose registries (for example 127/8, 10/8, 172.16/12,
  192.168/16, 169.254/16, ::1, fc00::/7, fe80::/10).
- Pin the connection to the validated address with the original host in Host
  and SNI. (Not possible on Workers; the plan records this nonconformance.)
- Never follow redirects, for deliveries and verification alike.
- Servers may keep an allowlist of callback URL patterns.
- HMAC is the only callback authentication; no bearer tokens.

### Errors

| Name | Code | Use |
|---|---|---|
| InvalidParams | -32602 | arguments not matching `inputSchema`, malformed or non-https `delivery.url`, invalid secret |
| NotFound | -32023 | unknown event name, or no subscription for the key on unsubscribe; `data.kind` `"event"` or `"subscription"` |
| Forbidden | -32024 | principal not permitted or access revoked; no authenticated principal on subscribe or unsubscribe |
| ResourceExhausted | -32025 | limit reached; `data.limit` names it, `data.max` optional |
| Unsupported | -32026 | well-formed but unsupported, e.g. `{ "feature": "deliveryMode", "value": "push" }` |
| CallbackEndpointError | -32027 | webhook endpoint failed verification or is unreachable; `data.reason` is a `lastError` category |
| HeaderMismatch | -32020 | missing or mismatched `Mcp-Name` (HTTP 400) |

The new codes are marked provisional by the source.

### Headers on Streamable HTTP

`Mcp-Name` must equal `params.name` on `events/poll`, `events/stream`,
`events/subscribe` and `events/unsubscribe` (not on `events/list`);
`Mcp-Method` carries the method on every request. Termination by mode: push
sends `notifications/events/terminated`, poll returns an error on the next
poll, webhook sends a signed `terminated` envelope.

## ChatGPT (`/mcp?profile=chatgpt`)

- Advertise top-level `capabilities.events = {}` in the `initialize` result
  (the example uses protocol version `2025-03-26`) or in the `server/discover`
  result of protocol `2026-07-28` (`resultType: "complete"`,
  `supportedVersions: ["2026-07-28"]`). The page names no query parameter;
  the `?profile=chatgpt` profile URL is this project's registration choice.
- Implement `events/list`, `events/subscribe`, `events/unsubscribe` on the
  same authenticated endpoint as the tools. Webhook delivery only: polling,
  streaming and the `gap` and `terminated` control notifications are not
  supported.
- `events/list`: `events` with `name`, `description`, `delivery`
  (`["webhook"]`), `inputSchema`, `payloadSchema`; `nextCursor` paging.
- `events/subscribe`: `name`, `arguments`, `delivery { mode, url, secret }`,
  `cursor`, `ttlMs` (prose only). Validate authorization, name and arguments,
  a `whsec_` secret decoding to 24 to 64 bytes, then verify the callback, then
  store. Result: `id`, `refreshBefore` (the granted expiration), `cursor`
  (`null` for non-replayable types), `truncated`. Deterministic id over
  principal, callback URL, name and arguments; idempotent; canonical JSON for
  arguments. Grant no more than `ttlMs` except a minimum lifetime; stop
  delivery when `refreshBefore` passes. ChatGPT refreshes before
  `refreshBefore` with the same identity and its last cursor; a replacement
  secret replaces the stored one, dual-signing for a short window.
- `events/unsubscribe`: `name`, `arguments`, `delivery { mode, url }`;
  idempotent, authorized against the account; result `{}`.
- Verification: before sending application data, send a signed request with
  a fresh, single-use, short-lived challenge; body
  `{"type":"verification","challenge":"..."}`; `webhook-id` unique to the
  verification (example `msg_verification_123`), plus `webhook-timestamp`,
  `webhook-signature`, `X-MCP-Subscription-Id`. Require a 2xx echoing
  `{"challenge": "..."}`, compared in constant time. Failure is JSON-RPC
  `-32015` (CallbackEndpointError) with `data.reason` such as
  `challenge_failed` or `timeout`. Cache success per principal and callback
  URL for a bounded period.
- Delivery: POST one event per request; headers `Content-Type:
  application/json`, `webhook-id` equal to the body's `eventId`,
  `webhook-timestamp`, `webhook-signature` (Standard Webhooks over event id,
  timestamp and the exact body bytes), `X-MCP-Subscription-Id`. Body:
  `eventId` (unique, kept across retries), `name`, `timestamp` (ISO 8601 with
  timezone), `data`, `cursor`; at most 256 KiB. 2xx acknowledges. Retry
  transient failures with exponential backoff and bounded attempts, fresh
  timestamp and signature each time; never retry `410` or `413`.
- Destinations: require HTTPS; resolve and validate at connection time and
  connect to the validated address (not possible on Workers; see the plan);
  block non-public addresses; never follow redirects; apply the same to
  verification requests. Event payloads carry data, never instructions for
  the model.
