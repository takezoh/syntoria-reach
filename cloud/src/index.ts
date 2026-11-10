import { nodeBroker } from "./broker";
import { createAccessVerifier, AccessError } from "./access";
import { mcp } from "./mcp";
import { nodeOf, parseNodes } from "./nodes";
import { bearer } from "./secret";

export { NodeBroker } from "./broker";

// Access owns MCP OAuth discovery, registration and the client token exchange.
// This origin accepts only Access assertions for the configured MCP application
// and owner. Node WebSockets use their own credentials and bypass Access OAuth.
let cached: { key: string; verifier: ReturnType<typeof createAccessVerifier> } | undefined;

function verifier(env: Env) {
  const key = JSON.stringify([env.ACCESS_TEAM_DOMAIN, env.ACCESS_AUD, env.ACCESS_OWNER_EMAIL]);
  if (cached?.key !== key) {
    cached = { key, verifier: createAccessVerifier({
      teamDomain: env.ACCESS_TEAM_DOMAIN,
      audience: env.ACCESS_AUD,
      ownerEmail: env.ACCESS_OWNER_EMAIL,
    }) };
  }
  return cached.verifier;
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

function text(status: number, body: string): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/plain; charset=utf-8" } });
}

async function protectedMcp(request: Request, env: Env): Promise<Response> {
  if (parseNodes(env.NODES) === null) return text(503, "NODES is missing or malformed\n");
  let access;
  try {
    access = verifier(env);
  } catch {
    return text(503, "Access settings are missing or malformed\n");
  }
  try {
    await access.authenticate(request.headers.get("Cf-Access-Jwt-Assertion"));
  } catch (error) {
    const status = error instanceof AccessError ? error.status : 503;
    const code = status === 401 ? "invalid_token" : status === 403 ? "forbidden" : "authorization_unavailable";
    return json(status, { error: code }, status === 503 ? { "Retry-After": "5" } : {});
  }
  return mcp(request, env, parseNodes(env.NODES)!);
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
    const { pathname } = new URL(request.url);
    switch (pathname) {
      case "/mcp":
        return protectedMcp(request, env);
      case "/connect":
        return connect(request, env);
      case "/":
        return text(200, "Syntoria Reach gateway. MCP endpoint: /mcp\n");
    }
    return text(404, "not found\n");
  },
} satisfies ExportedHandler<Env>;
