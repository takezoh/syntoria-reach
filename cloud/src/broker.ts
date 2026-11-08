import { DurableObject } from "cloudflare:workers";

// The node broker holds the WebSocket the Linux daemon opened and forwards
// requests over it. It keeps no state of its own beyond that connection:
// operations, workspaces and sessions live on Linux. A request waiting for a
// reply exists only in memory; if the reply does not come back, the caller
// learns that the outcome is unknown and reconciles it by operation ID.

export type ReachError = { code: string; message: string };
export type Reply = { result: unknown } | { error: ReachError };

// The gateway waits briefly; long work is returned as a job or session the
// caller polls.
const REPLY_TIMEOUT_MS = 30_000;

// One node per gateway for now.
const NODE_NAME = "default";

export function nodeBroker(env: Env): DurableObjectStub<NodeBroker> {
  return env.NODE.get(env.NODE.idFromName(NODE_NAME));
}

export function failure(code: string, message: string): Reply {
  return { error: { code, message } };
}

export function unknownOutcome(why: string): Reply {
  return failure(
    "result_unknown",
    `${why}. The request may or may not have run: check it with get_operation, or resend it with the same operation_id.`,
  );
}

type Pending = { ws: WebSocket; resolve: (reply: Reply) => void; timer: ReturnType<typeof setTimeout> };

export class NodeBroker extends DurableObject<Env> {
  private pending = new Map<string, Pending>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // The daemon's keepalive is answered without waking the object.
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  // Accepts the daemon's WebSocket. The Worker has checked the node token.
  // A new connection replaces the old one (the daemon reconnected).
  async fetch(request: Request): Promise<Response> {
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
      return new Response("expected a WebSocket upgrade", { status: 426 });
    }
    for (const old of this.ctx.getWebSockets()) {
      old.close(4000, "replaced by a new connection");
    }
    const { 0: client, 1: server } = new WebSocketPair();
    this.ctx.acceptWebSocket(server);
    return new Response(null, { status: 101, webSocket: client });
  }

  // Forwards one Reach method to the node and waits for its reply.
  async call(method: string, params: Record<string, unknown>): Promise<Reply> {
    const ws = this.ctx.getWebSockets().find((s) => s.readyState === WebSocket.OPEN);
    if (!ws) {
      return failure("node_offline", "The Reach node is not connected. Nothing was run.");
    }
    const id = crypto.randomUUID();
    const reply = new Promise<Reply>((resolve) => {
      const timer = setTimeout(() => this.settle(id, unknownOutcome("The node did not answer in time")), REPLY_TIMEOUT_MS);
      this.pending.set(id, { ws, resolve, timer });
    });
    try {
      ws.send(JSON.stringify({ id, method, params }));
    } catch {
      this.settle(id, failure("node_offline", "The Reach node is not connected. Nothing was run."));
    }
    return reply;
  }

  async webSocketMessage(_ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    if (typeof message !== "string") return;
    let reply: { id?: unknown; result?: unknown; error?: ReachError };
    try {
      reply = JSON.parse(message);
    } catch {
      return;
    }
    if (typeof reply.id !== "string") return;
    this.settle(reply.id, reply.error ? { error: reply.error } : { result: reply.result });
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string): Promise<void> {
    this.dropped(ws);
    try {
      ws.close(code === 1005 || code === 1006 ? 1000 : code, reason);
    } catch {
      // already closed
    }
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    this.dropped(ws);
  }

  // Requests in flight on a lost connection have an unknown outcome: the
  // node may have run them.
  private dropped(ws: WebSocket): void {
    for (const [id, p] of this.pending) {
      if (p.ws === ws) this.settle(id, unknownOutcome("The connection to the node closed before it answered"));
    }
  }

  private settle(id: string, reply: Reply): void {
    const p = this.pending.get(id);
    if (!p) return; // answered already, or the reply came after the timeout
    this.pending.delete(id);
    clearTimeout(p.timer);
    p.resolve(reply);
  }
}
