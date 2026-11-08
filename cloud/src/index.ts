import { nodeBroker } from "./broker";
import { createGateVerifier, GateError, type GateVerifier } from "./gate";
import { mcp } from "./mcp";
import { nodeOf, parseNodes } from "./nodes";
import { bearer, usable } from "./secret";

export { NodeBroker } from "./broker";

// The Reach gateway: a remote MCP endpoint (/mcp) for Syntoria Gate access
// tokens, and the endpoint the Linux daemons of the nodes connect to
// (/connect).
//
//   /mcp        MCP Streamable HTTP; needs a Gate access token of OWNER_SUBJECT
//   /.well-known/oauth-protected-resource[/mcp]  sends MCP hosts to Gate
//   /connect    a daemon's WebSocket; needs the token of a node in NODES
//
// Gate signs the person in, registers MCP hosts and approves or revokes each
// connection. The gateway holds no OAuth state: it checks every MCP request
// with Gate, and its MCP URL is the audience registered there.

const GATE_TIMEOUT_MS = 10_000; // Gate may be starting from zero instances

// One verifier per MCP URL, so the discovery of Gate's introspection
// endpoint is reused across requests.
const verifiers = new Map<string, GateVerifier>();

function verifier(env: Env, resource: string): GateVerifier {
  let v = verifiers.get(resource);
  if (!v) {
    v = createGateVerifier({
      issuer: env.GATE_ISSUER,
      audience: resource,
      clientId: env.GATE_CLIENT_ID,
      clientSecret: env.GATE_CLIENT_SECRET,
      timeoutMs: GATE_TIMEOUT_MS,
    });
    verifiers.set(resource, v);
  }
  return v;
}

function configured(env: Env): boolean {
  return !!env.GATE_ISSUER && !!env.GATE_CLIENT_ID && usable(env.GATE_CLIENT_SECRET) && !!env.OWNER_SUBJECT && parseNodes(env.NODES) !== null;
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

function text(status: number, body: string): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/plain; charset=utf-8" } });
}

const unconfigured = () => text(503, "the gateway's Gate or NODES settings are missing or malformed\n");

async function protectedMcp(request: Request, env: Env, origin: string): Promise<Response> {
  if (!configured(env)) {
    return unconfigured();
  }
  let principal;
  try {
    principal = await verifier(env, `${origin}/mcp`).authenticate(request.headers.get("Authorization"));
  } catch (error) {
    if (error instanceof GateError && error.status === 401) {
      return json(401, { error: error.code }, {
        "WWW-Authenticate": `Bearer resource_metadata="${origin}/.well-known/oauth-protected-resource/mcp"`,
      });
    }
    return json(503, { error: "authorization_unavailable" }, { "Retry-After": "5" });
  }
  // Gate approval does not admit a user here; only the configured person may
  // use Reach.
  if (principal.userId !== env.OWNER_SUBJECT) {
    return json(403, { error: "forbidden" });
  }
  return mcp(request, env, parseNodes(env.NODES)!);
}

function resourceMetadata(env: Env, origin: string): Response {
  if (!configured(env)) {
    return unconfigured();
  }
  return json(200, {
    resource: `${origin}/mcp`,
    authorization_servers: [env.GATE_ISSUER],
    scopes_supported: ["openid"],
    bearer_methods_supported: ["header"],
    resource_name: "Syntoria Reach",
  });
}

// A daemon's outbound WebSocket. Only a node's token admits it, as that
// node; the node's broker then forwards MCP requests over it.
async function connect(request: Request, env: Env): Promise<Response> {
  const nodes = parseNodes(env.NODES);
  if (nodes === null) {
    return text(503, "NODES is not configured on the gateway, or is malformed\n");
  }
  const token = bearer(request);
  const node = token === null ? null : await nodeOf(nodes, token);
  if (node === null) {
    return text(401, "unauthorized\n");
  }
  return nodeBroker(env, node).fetch(request);
}

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const { origin, pathname } = new URL(request.url);
    switch (pathname) {
      case "/mcp":
        return protectedMcp(request, env, origin);
      case "/.well-known/oauth-protected-resource":
      case "/.well-known/oauth-protected-resource/mcp":
        return resourceMetadata(env, origin);
      case "/connect":
        return connect(request, env);
      case "/":
        return text(200, "Syntoria Reach gateway. MCP endpoint: /mcp\n");
    }
    return text(404, "not found\n");
  },
} satisfies ExportedHandler<Env>;
