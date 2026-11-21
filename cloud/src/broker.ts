import { DurableObject } from "cloudflare:workers";
import * as events from "./events";

// A node broker holds the WebSocket one node's Linux daemon opened and
// forwards requests over it; there is one Durable Object per node, named by
// its node ID. It keeps no state of its own beyond that connection:
// operations, workspaces and sessions live on Linux. A request waiting for a
// reply exists only in memory; if the reply does not come back, the caller
// learns that the outcome is unknown and reconciles it by operation ID.

export type ReachError = { code: string; message: string };
export type Reply = { result: unknown } | { error: ReachError };

// The gateway waits briefly; long work is returned as a job or session the
// caller polls.
const REPLY_TIMEOUT_MS = 30_000;

export function nodeBroker(env: Env, node: string): DurableObjectStub<NodeBroker> {
  return env.NODE.get(env.NODE.idFromName(node));
}

export function failure(code: string, message: string): Reply {
  return { error: { code, message } };
}

export function unknownOutcome(why: string, method?: string): Reply {
  if (method === "call_controller_tool") {
    return failure(
      "result_unknown",
      `${why}. The Controller call may have run: Reach keeps no receipt of it, so check its effect with the Controller's own tools before calling it again.`,
    );
  }
  return failure(
    "result_unknown",
    `${why}. The request may or may not have run: check it with get_operation, or resend it with the same operation_id.`,
  );
}

type Pending = { ws: WebSocket; method: string; resolve: (reply: Reply) => void; timer: ReturnType<typeof setTimeout> };

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
    await this.change((state) => {
      const next = structuredClone(state);
      for (const g of Object.values(next.grants)) g.authorized = false;
      return [next, null];
    });
    this.ctx.waitUntil(this.reauthorize());
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
      const timer = setTimeout(() => this.settle(id, unknownOutcome("The node did not answer in time", method)), REPLY_TIMEOUT_MS);
      this.pending.set(id, { ws, method, resolve, timer });
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
    let reply: { id?: unknown; type?: unknown; result?: unknown; error?: ReachError };
    try {
      reply = JSON.parse(message);
    } catch {
      return;
    }
    if (reply.type === "turn_stop") {
      try {
        await events.acknowledge(reply as events.Stop,
          stop => this.change(state => events.admit(state, stop, Date.now())),
          eventId => _ws.send(JSON.stringify({type:"turn_stop_ack",eventId})),
        );
      } catch {
        _ws.close(1011, "event storage failed");
      }
      return;
    }
    if (typeof reply.id !== "string") return;
    this.settle(reply.id, reply.error ? { error: reply.error } : { result: reply.result });
  }

  async webSocketClose(ws: WebSocket, code: number, reason: string): Promise<void> {
    this.dropped(ws);
    await this.pause();
    try {
      ws.close(code === 1005 || code === 1006 ? 1000 : code, reason);
    } catch {
      // already closed
    }
  }

  async webSocketError(ws: WebSocket): Promise<void> {
    this.dropped(ws);
    await this.pause();
  }

  // The transaction owns the state and alarm together; no acknowledgement is
  // sent until the transaction has committed.
  private async change<T>(fn: (state: events.State) => [events.State, T]): Promise<T> {
    return this.ctx.storage.transaction(async tx => {
      const state = events.empty();
      state.seq = await tx.get<number>("seq") ?? 0;
      for (const [k,v] of await tx.list<events.Grant>({prefix:"grant/"})) state.grants[k.slice(6)] = v;
      for (const [k,v] of await tx.list<events.State["events"][string]>({prefix:"event/"})) state.events[k.slice(6)] = v;
      for (const [k,v] of await tx.list<events.State["dedupe"][string]>({prefix:"dedupe/"})) state.dedupe[k.slice(7)] = v;
      const [next, result] = fn(state);
      for (const [prefix, before, after] of [
        ["grant/", state.grants, next.grants],
        ["event/", state.events, next.events],
        ["dedupe/", state.dedupe, next.dedupe],
      ] as const) {
        for (const id of Object.keys(before)) if (!(id in after)) await tx.delete(prefix + id);
        for (const [id, value] of Object.entries(after)) {
          if (JSON.stringify(value) !== JSON.stringify((before as Record<string, unknown>)[id])) await tx.put(prefix + id, value);
        }
      }
      if (next.seq !== state.seq) await tx.put("seq", next.seq);
      const deadlines = Object.values(next.grants).map(g => g.expires);
      if (deadlines.length) await tx.setAlarm(Math.min(...deadlines));
      else await tx.deleteAlarm();
      return result;
    });
  }

  async alarm(): Promise<void> {
    await this.change(state => [events.expire(state, Date.now()), null]);
  }

  private async authorize(input: events.GrantInput, expiry: number): Promise<'allowed' | 'denied' | 'unavailable'> {
    const reply = await this.call("events.authorize", {
      session_id: input.arguments.session_id,
      subscription_key: events.key(input),
      expires_at: new Date(expiry).toISOString(),
    });
    if ('result' in reply && reply.result && typeof reply.result === 'object' && 'decision' in reply.result) {
      const decision = reply.result.decision;
      if (decision === 'allowed' || decision === 'denied') return decision;
    }
    return 'unavailable';
  }

  async subscribe(input: events.GrantInput): Promise<events.Result<events.Grant>> {
    if (!events.valid(input)) return {ok:false,error:'invalid_params'};
    const now = Date.now();
    const ws = this.ctx.getWebSockets().find(s => s.readyState === WebSocket.OPEN);
    const answer = await this.authorize(input, now + 300_000);
    const decision = ws && ws.readyState === WebSocket.OPEN && this.ctx.getWebSockets().includes(ws) ? answer : 'unavailable';
    return this.change(state => events.subscribe(state, input, Date.now(), crypto.randomUUID(), decision));
  }

  async refresh(input: events.GrantInput): Promise<events.Result<events.Grant>> {
    return this.subscribe(input);
  }

  async unsubscribe(input: events.GrantInput): Promise<events.Result<null>> {
    if (!events.valid(input)) return {ok:false,error:'invalid_params'};
    return this.change(state => events.unsubscribe(state, input, Date.now()));
  }

  async poll(input: events.GrantInput, cursor: string | null, batch: number, maxAgeMs?: number): Promise<events.Result<{events:events.Stop[];cursor:string;truncated:boolean;hasMore:boolean;nextPollMs:number}>> {
    if (!events.valid(input) || (cursor !== null && typeof cursor !== 'string')) return {ok:false,error:'invalid_params'};
    const grant = await this.subscribe(input);
    if (!grant.ok) return grant;
    return this.change(state => events.poll(state, input, cursor, batch, Date.now(), crypto.randomUUID(), maxAgeMs));
  }

  private async pause(): Promise<void> {
    if (this.ctx.getWebSockets().some(ws => ws.readyState === WebSocket.OPEN)) return;
    await this.change(state => {
      const next = structuredClone(state);
      for (const g of Object.values(next.grants)) g.authorized = false;
      return [next, null];
    });
  }

  private async reauthorize(): Promise<void> {
    const grants = await this.ctx.storage.list<events.Grant>({prefix:"grant/"});
    for (const g of grants.values()) {
      if (g.expires <= Date.now()) continue;
      const decision = await this.authorize(g, g.expires);
      await this.change(current => [events.setAuthorization(current, g.id, decision, Date.now()), null]);
    }
  }

  // Requests in flight on a lost connection have an unknown outcome: the
  // node may have run them.
  private dropped(ws: WebSocket): void {
    for (const [id, p] of this.pending) {
      if (p.ws === ws) this.settle(id, unknownOutcome("The connection to the node closed before it answered", p.method));
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
