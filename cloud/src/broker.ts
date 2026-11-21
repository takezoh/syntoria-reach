import { DurableObject } from "cloudflare:workers";
import * as events from "./events";
import {destination, safeHttps, secretBytes, type SafeHttps} from './webhook';

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
  private waiters = new Set<() => void>();
  private readerVersion = 0;
  private readerEnds = new Map<string, 'forbidden' | 'expired' | 'limit' | 'closed'>();

  protected webhookPort(): SafeHttps {
    return safeHttps((this.env as Env & {WEBHOOK_ALLOWED_ORIGINS?:string}).WEBHOOK_ALLOWED_ORIGINS);
  }

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // The daemon's keepalive is answered without waking the object.
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
    ctx.blockConcurrencyWhile(async () => { await this.change(s => [events.removeReaders(s), null]); });
  }

  private wakeReaders(): void { this.readerVersion++; for (const wake of this.waiters) wake(); this.waiters.clear(); }

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
    this.wakeReaders();
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
        const randomIds = Array.from({length:events.MAX_GRANTS}, () => Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2,'0')).join(''));
        await events.acknowledge(reply as events.Stop,
          stop => this.change(state => {
            const ids:Record<string,string> = {};
            Object.values(state.grants).filter(g => g.mode === 'webhook').forEach((g,i) => { ids[g.id] = randomIds[i]; });
            return events.admit(state, stop, Date.now(), ids);
          }, 'limit'),
          eventId => _ws.send(JSON.stringify({type:"turn_stop_ack",eventId})),
        );
        this.wakeReaders();
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
  private async change<T>(fn: (state: events.State) => [events.State, T], reason?: 'forbidden' | 'limit' | 'closed'): Promise<T> {
    const changed = await this.ctx.storage.transaction(async tx => {
      const state = events.empty();
      state.seq = await tx.get<number>("seq") ?? 0;
      for (const [k,v] of await tx.list<events.Message>({prefix:"message/"})) state.messages[k.slice(8)] = v;
      for (const [k,v] of await tx.list<events.Grant>({prefix:"grant/"})) state.grants[k.slice(6)] = v;
      for (const [k,v] of await tx.list<events.State["events"][string]>({prefix:"event/"})) state.events[k.slice(6)] = v;
      for (const [k,v] of await tx.list<events.State["dedupe"][string]>({prefix:"dedupe/"})) state.dedupe[k.slice(7)] = v;
      for (const [k,v] of await tx.list<events.Verification>({prefix:"verification/"})) state.pending[k.slice(13)] = v;
      for (const [k,v] of await tx.list<events.Control>({prefix:"control/"})) state.controls[k.slice(8)] = v;
      for (const [k,v] of await tx.list<number>({prefix:"proof/"})) state.cache[k.slice(6)] = v;
      const [next, result] = fn(state);
      for (const [prefix, before, after] of [
        ["message/", state.messages, next.messages],
        ["grant/", state.grants, next.grants],
        ["event/", state.events, next.events],
        ["dedupe/", state.dedupe, next.dedupe],
        ["verification/", state.pending, next.pending],
        ["control/", state.controls, next.controls],
        ["proof/", state.cache, next.cache],
      ] as const) {
        for (const id of Object.keys(before)) if (!(id in after)) await tx.delete(prefix + id);
        for (const [id, value] of Object.entries(after)) {
          if (JSON.stringify(value) !== JSON.stringify((before as Record<string, unknown>)[id])) await tx.put(prefix + id, value);
        }
      }
      if (next.seq !== state.seq) await tx.put("seq", next.seq);
      const deadlines = [...Object.values(next.messages).filter(m => next.grants[m.grant]?.authorized && next.grants[m.grant]?.deliveryActive !== false).map(m => m.due), ...Object.values(next.controls).flatMap(c => c.body.type === 'terminated' || next.grants[c.subscription]?.authorized ? [c.due,c.expires] : [c.expires]), ...Object.values(next.cache), ...Object.values(next.grants).map(g => g.expires), ...Object.values(next.dedupe).map(d => d.admitted + events.DEDUPE_MS), ...Object.values(next.pending).map(p => p.expires)];
      if (deadlines.length) await tx.setAlarm(Math.min(...deadlines));
      else await tx.deleteAlarm();
      const ended = Object.values(state.grants).filter(g => g.reader && !next.grants[g.id]).map(g => ({id:g.id, reason:g.expires <= Date.now() ? 'expired' as const : reason}));
      return {result, ended};
    });
    for (const end of changed.ended) if (end.reason) this.readerEnds.set(end.id, end.reason);
    if (changed.ended.length) this.wakeReaders();
    return changed.result;
  }

  async alarm(): Promise<void> {
    for (;;) {
      const control = await this.change(state => {
        const n = events.expire(state,Date.now());
        const c = Object.values(n.controls).find(x => x.due <= Date.now() && (x.body.type === 'terminated' || n.grants[x.subscription]?.authorized));
        if (!c) return [n,null];
        if (c.attempts >= 10) { delete n.controls[c.id]; return [n,null]; }
        c.attempts++; c.due = Date.now() + Math.min(1000 * 2 ** (c.attempts-1),60000);
        return [n,structuredClone(c)];
      });
      if (!control) break;
      const outcome = await this.webhookPort().deliver(control.url,control.secret,control.subscription,control.id,Date.now(),JSON.stringify(control.body),control.expires,async () => {
        const current = await this.ctx.storage.get<events.Control>('control/' + control.id);
        if (!current || current.attempts !== control.attempts) return false;
        if (current.body.type === 'terminated') return true;
        const grant = await this.ctx.storage.get<events.Grant>('grant/' + current.subscription);
        return !!grant && grant.authorized && grant.expires > Date.now();
      });
      await this.change(state => {
        const n=events.expire(state,Date.now()), c=n.controls[control.id];
        if (c && c.attempts === control.attempts) {
          if (outcome.status === 'accepted' || outcome.status === 'terminal' || (outcome.status === 'retryable' && c.attempts >= 10)) delete n.controls[c.id];
          else if (outcome.status === 'deferred') c.attempts--;
        }
        return [n,null];
      });
    }
    for (;;) {
      const reserved = await this.change(state => events.reserve(state, Date.now()));
      if (!reserved) break;
      const {key,message,grant,stop,cursor} = reserved;
      const body = JSON.stringify({eventId:stop.eventId,name:grant.name,timestamp:stop.observed_at,data:{node:grant.node,session_id:stop.session_id,state:stop.state,observed_at:stop.observed_at},cursor});
      const outcome = await this.webhookPort().deliver(grant.callback!,grant.secret!,grant.id,message.id,Date.now(),body,grant.expires,async () => {
        const current = await this.ctx.storage.get<events.Grant>('grant/' + grant.id);
        return !!current && current.authorized && current.expires > Date.now();
      });
      await this.change(state => [events.delivered(events.expire(state,Date.now()),key,message.attempts,outcome),null]);
    }
    this.wakeReaders();
  }

  private async authorize(input: events.GrantInput, expiry: number): Promise<'allowed' | 'denied' | 'unavailable'> {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(events.key(input)));
    const subscriptionKey = Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, '0')).join('');
    const reply = await this.call("events.authorize", {
      session_id: input.arguments.session_id,
      subscription_key: subscriptionKey,
      expires_at: new Date(expiry).toISOString(),
    });
    if ('result' in reply && reply.result && typeof reply.result === 'object' && 'decision' in reply.result) {
      const decision = reply.result.decision;
      if (decision === 'allowed' || decision === 'denied') return decision;
    }
    return 'unavailable';
  }

  async subscribe(input: events.GrantInput, options: events.WebhookOptions = {}): Promise<events.Result<Omit<events.Grant, 'secret'> | events.WebhookResult>> {
    if (!events.valid(input) || (input.mode === 'webhook' && events.lease(options) === null)) return {ok:false,error:'invalid_params'};
    if (input.mode === 'webhook') {
      if (!secretBytes(input.secret)) return {ok:false,error:'invalid_params'};
      const allowed = destination(input.callback!, (this.env as Env & {WEBHOOK_ALLOWED_ORIGINS?:string}).WEBHOOK_ALLOWED_ORIGINS);
      if (!allowed.ok) return allowed;
    }
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(events.key(input)));
    const subscriptionId = 'sub_' + Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, '0')).join('');
    const now = Date.now();
    const ws = this.ctx.getWebSockets().find(s => s.readyState === WebSocket.OPEN);
    const deadline = now + (input.mode === 'webhook' ? events.lease(options)! : events.LEASE_MS);
    const answer = await this.authorize(input, deadline);
    const decision = ws && ws.readyState === WebSocket.OPEN && this.ctx.getWebSockets().includes(ws) ? answer : 'unavailable';
    if (input.mode !== 'webhook') return this.change(state => events.subscribe(state, input, Date.now(), subscriptionId, decision));
    const pendingKey = events.verificationKey(input);
    const id = crypto.randomUUID(), nonce = crypto.randomUUID() + crypto.randomUUID(), terminatedId = `msg_terminated_${crypto.randomUUID()}`;
    const reservedAt = Date.now();
    const reserved = await this.change<events.Result<string>>(state => {
      const next = events.expire(state, reservedAt);
      const existing = Object.values(next.grants).find(g => !g.reader && events.key(g) === events.key(input));
      if (decision === 'denied') { delete next.pending[pendingKey]; return [existing ? events.deny(next,existing.id,reservedAt,terminatedId) : next,{ok:false,error:'forbidden'}]; }
      if (!existing && decision !== 'allowed') return [next,{ok:false as const,error:'unavailable' as const}];
      if (next.cache[pendingKey] > reservedAt) return [next,{ok:true as const,value:''}];
      if (next.pending[pendingKey]) return [next,{ok:false as const,error:'unavailable' as const}];
      if (!existing && Object.keys(next.grants).length + Object.keys(next.pending).length >= events.MAX_GRANTS) return [next,{ok:false as const,error:'limit' as const}];
      next.pending[pendingKey] = {id,key:events.key(input),nonce,secret:input.secret!,expires:existing ? Math.min(existing.expires,reservedAt+events.LEASE_MS) : reservedAt+events.LEASE_MS};
      return [next,{ok:true as const,value:existing?.id ?? subscriptionId}];
    });
    if (!reserved.ok) return reserved;
    const checked = reserved.value === '' ? {ok:true as const,value:null} : await this.webhookPort().verify(input.callback!,input.secret!,reserved.value,nonce,`msg_verification_${crypto.randomUUID()}`,Date.now());
    const completedAt = Date.now();
    const currentDecision = ws && ws.readyState === WebSocket.OPEN && this.ctx.getWebSockets().includes(ws) ? decision : 'unavailable';
    return this.change<events.Result<events.WebhookResult>>(state => {
      const next = events.expire(state,completedAt);
      if (reserved.value !== '') {
        if (next.pending[pendingKey]?.id !== id) return [next,{ok:false,error:'unavailable'}];
        delete next.pending[pendingKey];
      } else if (!(next.cache[pendingKey] > completedAt)) return [next,{ok:false,error:'unavailable'}];
      if (!checked.ok) return [next,checked];
      if (completedAt >= deadline) return [next,{ok:false,error:'unavailable'}];
      const [updated,result] = events.subscribe(next,input,completedAt,subscriptionId,currentDecision,options,deadline);
      if (!result.ok) return [updated,result];
      if (reserved.value !== '') updated.cache[pendingKey] = result.value.expires;
      const value = events.webhookResult(updated,result.value);
      result.value.truncated = false;
      return [updated,{ok:true,value}];
    });
  }

  async refresh(input: events.GrantInput, options: events.WebhookOptions = {}): Promise<events.Result<Omit<events.Grant, 'secret'> | events.WebhookResult>> {
    return this.subscribe(input, options);
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

  // Newline-delimited records are active, event, keepalive, or terminated.
  // The wire closes the reader on every request exit, including abort.
  async openReader(input: events.GrantInput, cursor: string | null): Promise<events.Result<ReadableStream<Uint8Array>>> {
    if (!events.valid(input) || input.mode !== 'push' || (cursor !== null && typeof cursor !== 'string')) return {ok:false,error:'invalid_params'};
    const ws = this.ctx.getWebSockets().find(s => s.readyState === WebSocket.OPEN);
    const decision = await this.authorize(input, Date.now() + events.LEASE_MS);
    const active = ws && ws.readyState === WebSocket.OPEN && this.ctx.getWebSockets().includes(ws);
    const id = crypto.randomUUID();
    const opened = await this.change(s => events.openReader(s, input, Date.now(), id, active ? decision : 'unavailable', cursor));
    if (!opened.ok) return opened;
    const encoder = new TextEncoder();
    let initial = true;
    let closed = false;
    let seenVersion = -1;
    const cleanup = async () => {
      if (closed) return;
      closed = true; this.wakeReaders();
      await this.change(s => [events.closeReader(s, id), null], 'closed');
      this.readerEnds.delete(id);
    };
    const stream = new ReadableStream<Uint8Array>({
      pull: async controller => {
        if (initial) {
          initial = false;
          controller.enqueue(encoder.encode(JSON.stringify({type:'active',id,cursor:opened.value.cursor,truncated:opened.value.truncated})+'\n'));
          return;
        }
        while (!closed) {
          let wake!: () => void;
          const signal = new Promise<void>(resolve => { wake = resolve; this.waiters.add(resolve); });
          try {
            const version = this.readerVersion;
            const result = seenVersion !== version || Date.now() >= opened.value.grant.expires ? await this.change(s => {
              if (this.ctx.getWebSockets().some(ws => ws.readyState === WebSocket.OPEN)) return events.readReader(s, id, Date.now());
              const next = events.expire(s, Date.now());
              return [next, {ok:false as const,error:next.grants[id] ? 'unavailable' as const : 'not_found' as const}];
            }) : {ok:true as const,value:null};
            if (!result.ok && result.error !== 'unavailable') {
              const reason = this.readerEnds.get(id) ?? 'expired';
              if (reason !== 'closed') controller.enqueue(encoder.encode(JSON.stringify({type:'terminated',reason})+'\n'));
              await cleanup(); controller.close(); return;
            }
            if (result.ok && result.value) {
              controller.enqueue(encoder.encode(JSON.stringify({type:'event',...result.value})+'\n'));
              return;
            }
            seenVersion = version;
            let timer: ReturnType<typeof setTimeout> | undefined;
            const heartbeat = new Promise<'heartbeat'>(resolve => { timer = setTimeout(() => resolve('heartbeat'), 1000); });
            const outcome = await Promise.race([signal.then(() => 'wake' as const), heartbeat]);
            if (timer !== undefined) clearTimeout(timer);
            if (outcome === 'heartbeat' && !closed) {
              controller.enqueue(encoder.encode('{"type":"keepalive"}\n'));
              return;
            }
          } finally { this.waiters.delete(wake); }
        }
      },
      cancel: cleanup,
    }, {highWaterMark:0});
    return {ok:true,value:stream};
  }

  async closeReader(input: events.GrantInput, id: string): Promise<events.Result<null>> {
    if (!events.valid(input) || input.mode !== 'push' || typeof id !== 'string') return {ok:false,error:'invalid_params'};
    return this.change(s => {
      const g = s.grants[id];
      if (!g || !g.reader || events.key(g) !== events.key(input)) return [s,{ok:false,error:'not_found'}] as [events.State,events.Result<null>];
      return [events.closeReader(s,id),{ok:true,value:null}];
    }, 'closed');
  }

  private async pause(): Promise<void> {
    if (this.ctx.getWebSockets().some(ws => ws.readyState === WebSocket.OPEN)) return;
    await this.change(state => {
      const next = structuredClone(state);
      for (const g of Object.values(next.grants)) g.authorized = false;
      return [next, null];
    });
    this.wakeReaders();
  }

  private async reauthorize(): Promise<void> {
    const ws = this.ctx.getWebSockets().find(s => s.readyState === WebSocket.OPEN);
    if (!ws) return;
    const grants = await this.ctx.storage.list<events.Grant>({prefix:"grant/"});
    for (const g of grants.values()) {
      if (g.expires <= Date.now()) continue;
      const decision = await this.authorize(g, g.expires);
      if (ws.readyState !== WebSocket.OPEN || !this.ctx.getWebSockets().includes(ws)) return;
      const terminatedId = `msg_terminated_${crypto.randomUUID()}`;
      await this.change(current => [decision === 'denied' ? events.deny(current,g.id,Date.now(),terminatedId) : events.setAuthorization(current, g.id, decision, Date.now()), null], decision === 'denied' ? 'forbidden' : undefined);
      this.wakeReaders();
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
