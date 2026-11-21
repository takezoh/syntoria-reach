// Private per-node limits: 128 grants, 256 retained occurrences per grant,
// 100 events per poll, and 2048 dedupe IDs per node for at most 24 hours.
// A later resend, or one evicted by the cap, can be delivered again with the
// same eventId. Only grants live at the observation time are eligible.
export const MAX_GRANTS = 128;
export const MAX_EVENTS = 256;
export const MAX_BATCH = 100;
export const MAX_DEDUPE = 2048;
export const DEDUPE_MS = 24 * 60 * 60 * 1000;
export const LEASE_MS = 300_000;
export type WebhookOptions = { ttlMs?: number | null; cursor?: string | null; maxAgeMs?: number };
export function lease(options: WebhookOptions): number | null {
  const { ttlMs, cursor, maxAgeMs } = options;
  if (ttlMs != null && (!Number.isSafeInteger(ttlMs) || ttlMs < 0)) return null;
  if (cursor != null && typeof cursor !== "string") return null;
  if (maxAgeMs !== undefined && (!Number.isSafeInteger(maxAgeMs) || maxAgeMs < 0)) return null;
  return Math.max(60_000, Math.min(ttlMs ?? LEASE_MS, LEASE_MS));
}
// Node and Cloudflare clocks are assumed NTP-synchronized. The 30-second
// tolerance favors delivery: an early stop can duplicate the read-once result,
// whereas dropping a real stop loses its notification.
export const CLOCK_SKEW_MS = 30_000;

export type Principal = { issuer: string; subject: string };
// The wire selects the profile per registration URL; each has its own webhook-id rule.
export type Profile = 'standard' | 'chatgpt';
export type GrantInput = { principal: Principal; node: string; name: string; arguments: { node: string; session_id: string }; profile: Profile; mode: 'poll' | 'push' | 'webhook'; callback?: string; secret?: string };
export type Stop = { type: 'turn_stop'; eventId: string; session_id: string; state: 'idle' | 'waiting_on_human' | 'exited' | 'unknown'; observed_at: string };
export type Grant = GrantInput & { id: string; created: number; expires: number; authorized: boolean; events: string[]; floor: number; reader?: true; deliveryActive?: boolean; lastOutcome?: DeliveryCategory | null; truncated?: boolean };
type Entry = { stop: Stop; seq: number; grants: string[] };
type Dedupe = { admitted: number; grants: string[] };
export type Verification = { id:string; key:string; nonce:string; secret:string; expires:number };
export type DeliveryCategory = 'callback_endpoint' | 'resolution_failed' | 'timeout' | 'tls_error' | 'connection_refused' | 'http_4xx' | 'http_5xx' | 'http_other';
export type PublicError = 'connection_refused' | 'timeout' | 'tls_error' | 'http_4xx' | 'http_5xx' | 'challenge_failed';
// No callback connection, including a forbidden destination or DNS failure,
// maps to connection_refused; other non-2xx below 500 maps to http_4xx.
export function publicError(error: DeliveryCategory): PublicError {
  if (error === 'callback_endpoint' || error === 'resolution_failed') return 'connection_refused';
  if (error === 'http_other') return 'http_4xx';
  return error;
}
export type WebhookResult = {id:string; refreshBefore:string; cursor:string; truncated:boolean; deliveryStatus:{active:boolean;lastError:PublicError|null}};
export function webhookResult(s: State, g: Grant): WebhookResult {
  return {id:g.id,refreshBefore:new Date(g.expires).toISOString(),cursor:watermark(s,g),truncated:!!g.truncated,deliveryStatus:{active:g.deliveryActive !== false,lastError:g.lastOutcome ? publicError(g.lastOutcome) : null}};
}
export function watermark(s:State,g:Grant):string {
  const messages = Object.values(s.messages).filter(m => m.grant === g.id);
  const position = messages.length ? Math.min(...messages.map(m => m.seq)) - 1 : s.seq;
  return btoa(JSON.stringify({grant:g.id,position}));
}
export type DeliveryResult = {status:'accepted'} | {status:'deferred'} | {status:'terminal'|'retryable'; category:DeliveryCategory};
export type Message = { grant:string; eventId:string; id:string; seq:number; attempts:number; due:number; lastOutcome:DeliveryCategory | null };
export type Control = {subscription:string; url:string; secret:string; id:string; expires:number; attempts:number; due:number; body:{type:'gap';cursor:string}|{type:'terminated';error:{code:number;message:string}}};
export type State = { messages: Record<string, Message>; controls:Record<string,Control>; cache:Record<string,number>; grants: Record<string, Grant>; events: Record<string, Entry>; dedupe: Record<string, Dedupe>; pending: Record<string, Verification>; seq: number };
export type Category = 'unavailable' | 'forbidden' | 'not_found' | 'invalid_params' | 'limit' | 'callback_endpoint' | 'resolution_failed' | 'timeout' | 'tls_error' | 'connection_refused' | 'challenge_failed';
export type Result<T> = { ok: true; value: T } | { ok: false; error: Category };
export const empty = (): State => ({ messages: {}, controls:{}, cache:{}, grants: {}, events: {}, dedupe: {}, pending: {}, seq: 0 });
export const key = (g: GrantInput): string => JSON.stringify([g.principal.issuer, g.principal.subject, g.node, g.name, g.arguments.node, g.arguments.session_id, g.profile, g.mode, g.mode === 'webhook' ? g.callback : null]);
export function valid(g: GrantInput): boolean {
  return !!(g && g.principal && typeof g.principal.issuer === 'string' && g.principal.issuer &&
    typeof g.principal.subject === 'string' && g.principal.subject && typeof g.node === 'string' && g.node &&
    g.name === 'agent.session_state_changed' && g.arguments && typeof g.arguments === 'object' &&
    Object.keys(g.arguments).length === 2 && g.arguments.node === g.node &&
    typeof g.arguments.session_id === 'string' && g.arguments.session_id &&
    (g.profile === 'standard' || g.profile === 'chatgpt') && ['poll', 'push', 'webhook'].includes(g.mode) &&
    (g.mode === 'webhook' ? (typeof g.callback === 'string' && !!g.callback) : g.secret === undefined));
}
export const verificationKey = (g: GrantInput):string => JSON.stringify([g.principal.issuer,g.principal.subject,g.callback]);
function remove(s: State, id: string): void {
  const g = s.grants[id]; if (!g) return;
  // A pending challenge for the same principal and URL may belong to another
  // session's subscribe; delete only the one reserved for this grant.
  if (g.mode === 'webhook' && s.pending[verificationKey(g)]?.id === id) delete s.pending[verificationKey(g)];
  for (const eventId of g.events) {
    const e = s.events[eventId]; if (!e) continue;
    e.grants = e.grants.filter(x => x !== id);
    if (!e.grants.length) delete s.events[eventId];
  }
  for (const d of Object.values(s.dedupe)) {
    d.grants = d.grants.filter(g => g !== id);
  }
  for (const [key, message] of Object.entries(s.messages)) if (message.grant === id) delete s.messages[key];
  for (const [key, control] of Object.entries(s.controls)) if (control.subscription === id && control.body.type === 'gap') delete s.controls[key];
  delete s.grants[id];
  if (g.mode === 'webhook' && !Object.values(s.grants).some(x => x.mode === 'webhook' && verificationKey(x) === verificationKey(g))) delete s.cache[verificationKey(g)];
}
export function deny(s:State,id:string,now:number,messageId:string):State {
  const n=expire(s,now), g=n.grants[id];
  if (g && g.mode === 'webhook' && g.profile === 'standard') n.controls[messageId] = {subscription:g.id,url:g.callback!,secret:g.secret!,id:messageId,expires:g.expires,attempts:0,due:now,body:{type:'terminated',error:{code:-32024,message:'Forbidden'}}};
  remove(n,id); return n;
}
export function removeReaders(s: State): State {
  const n = structuredClone(s);
  for (const g of Object.values(n.grants)) if (g.reader) remove(n, g.id);
  return n;
}
export function closeReader(s: State, id: string): State {
  const n = structuredClone(s); remove(n, id); return n;
}
export function openReader(s: State, input: GrantInput, now: number, id: string, decision: 'allowed' | 'denied' | 'unavailable', cursor: string | null): [State, Result<{grant: Grant; cursor: string; truncated: boolean}>] {
  const n = expire(s, now);
  if (!valid(input) || input.mode !== 'push' || (cursor !== null && typeof cursor !== 'string')) return [n,{ok:false,error:'invalid_params'}];
  if (decision !== 'allowed') return [n,{ok:false,error:decision === 'denied' ? 'forbidden' : 'unavailable'}];
  if (Object.keys(n.grants).length >= MAX_GRANTS) return [n,{ok:false,error:'limit'}];
  const grant: Grant = {...structuredClone(input), id, created:now, expires:now+LEASE_MS, authorized:true, events:[], floor:n.seq, reader:true};
  n.grants[id] = grant;
  return [n,{ok:true,value:{grant,cursor:btoa(JSON.stringify({grant:id,position:n.seq})),truncated:cursor !== null}}];
}
export function readReader(s: State, id: string, now: number): [State, Result<{event: Stop; cursor: string} | null>] {
  const n = expire(s, now); const g = n.grants[id];
  if (!g) return [n,{ok:false,error:'not_found'}];
  if (!g.authorized) return [n,{ok:false,error:'unavailable'}];
  const eventId = g.events.shift();
  if (!eventId) return [n,{ok:true,value:null}];
  const row = n.events[eventId];
  g.floor = row.seq; row.grants = row.grants.filter(x => x !== id);
  if (!row.grants.length) delete n.events[eventId];
  return [n,{ok:true,value:{event:row.stop,cursor:btoa(JSON.stringify({grant:id,position:row.seq}))}}];
}
export function expire(s: State, now: number): State {
  const n = structuredClone(s);
  for (const g of Object.values(n.grants)) if (g.expires <= now) remove(n, g.id);
  for (const [id, pending] of Object.entries(n.pending)) if (pending.expires <= now) delete n.pending[id];
  for (const [id, control] of Object.entries(n.controls)) if (control.expires <= now) delete n.controls[id];
  for (const [id, until] of Object.entries(n.cache)) if (until <= now || !Object.values(n.grants).some(g => g.mode === 'webhook' && verificationKey(g) === id)) delete n.cache[id];
  for (const [id, d] of Object.entries(n.dedupe)) if (d.admitted + DEDUPE_MS <= now) delete n.dedupe[id];
  return n;
}
export function subscribe(s: State, input: GrantInput, now: number, id: string, decision: 'allowed' | 'denied' | 'unavailable', options: WebhookOptions = {}, deadline = now + LEASE_MS): [State, Result<Grant>] {
  let n = expire(s, now);
  if (!valid(input) || (input.mode === 'webhook' && lease(options) === null)) return [n, {ok:false,error:'invalid_params'}];
  const existing = Object.values(n.grants).find(g => !g.reader && key(g) === key(input));
  if (existing) {
    if (decision === 'denied') { remove(n, existing.id); return [n, {ok:false,error:'forbidden'}]; }
    existing.expires = deadline;
    if (input.mode === 'webhook' && options.cursor != null) {
      let c: {grant:string;position:number} | undefined;
      try { c = JSON.parse(atob(options.cursor)); } catch { /* invalid cursor */ }
      const current = JSON.parse(atob(watermark(n, existing))) as {position:number};
      if (!c || c.grant !== existing.id || !Number.isSafeInteger(c.position) || c.position < 0 || c.position > current.position) existing.truncated = true;
    }
    existing.authorized = decision === 'allowed';
    if (input.mode === 'webhook') {
      existing.secret = input.secret; existing.deliveryActive = true;
      for (const control of Object.values(n.controls)) if (control.subscription === existing.id && control.body.type === 'gap') { control.secret = input.secret!; control.expires = existing.expires; }
    }
    return [n, {ok:true,value:existing}];
  }
  if (decision !== 'allowed') return [n, {ok:false,error:decision === 'denied' ? 'forbidden' : 'unavailable'}];
  if (Object.keys(n.grants).length >= MAX_GRANTS) return [n, {ok:false,error:'limit'}];
  const g: Grant = { ...structuredClone(input), id, created:now, expires:deadline, authorized:true, events:[], floor:n.seq, truncated:input.mode === 'webhook' && options.cursor != null };
  n.grants[id] = g;
  return [n, {ok:true,value:g}];
}
export function setAuthorization(s: State, id: string, decision: 'allowed' | 'denied' | 'unavailable', now: number): State {
  const n = expire(s, now); const g = n.grants[id];
  if (g) { if (decision === 'denied') remove(n,id); else if (decision === 'allowed') g.authorized = true; }
  return n;
}
export function unsubscribe(s: State, input: GrantInput, now: number): [State, Result<null>] {
  const n = expire(s,now); const g = Object.values(n.grants).find(x => !x.reader && key(x) === key(input));
  // Unsubscribing also cancels this subscription's in-flight challenge, but not
  // another session's challenge to the same principal and URL.
  if (input.mode === 'webhook' && n.pending[verificationKey(input)]?.key === key(input)) delete n.pending[verificationKey(input)];
  if (!g) return [n,{ok:false,error:'not_found'}];
  remove(n,g.id); return [n,{ok:true,value:null}];
}
export function admit(s: State, stop: Stop, now: number, ids: Record<string,string> = {}): [State, boolean] {
  const n = expire(s,now);
  if (!stop.eventId || !stop.session_id || !['idle','waiting_on_human','exited','unknown'].includes(stop.state) || !Number.isFinite(Date.parse(stop.observed_at))) return [n,false];
  if (n.events[stop.eventId] || n.dedupe[stop.eventId]) return [n,true];
  const matching = Object.values(n.grants).filter(g => g.arguments.session_id === stop.session_id && Date.parse(stop.observed_at) >= g.created - CLOCK_SKEW_MS);
  for (const g of matching) {
    if (g.events.length >= MAX_EVENTS) {
      if (g.reader) { remove(n, g.id); continue; }
      const oldest = g.events.shift()!;
      delete n.messages[JSON.stringify([g.id,oldest])];
      const row = n.events[oldest];
      g.floor = Math.max(g.floor, row.seq);
      row.grants = row.grants.filter(id => id !== g.id);
      if (!row.grants.length) delete n.events[oldest];
      if (g.mode === 'webhook') {
        g.truncated = true;
        if (g.profile === 'standard') {
          const control = Object.values(n.controls).find(c => c.subscription === g.id && c.body.type === 'gap' && c.attempts === 0);
          const id = control?.id ?? `msg_gap_${ids[g.id]}`;
          if (!ids[g.id]) throw new Error('missing gap message id');
          n.controls[id] = control ?? {subscription:g.id,url:g.callback!,secret:g.secret!,id,expires:g.expires,attempts:0,due:now,body:{type:'gap',cursor:''}};
          n.controls[id].body = {type:'gap',cursor:watermark(n,g)};
        }
      }
    }
  }
  const eligible = matching.filter(g => !!n.grants[g.id]);
  if (eligible.length) {
    n.events[stop.eventId] = { stop, seq: ++n.seq, grants: eligible.map(g => g.id) };
    for (const g of eligible) {
      g.events.push(stop.eventId);
      if (g.mode === 'webhook') {
        const id = g.profile === 'chatgpt' ? stop.eventId : ids[g.id];
        if (!id) throw new Error('missing webhook message id');
        n.messages[JSON.stringify([g.id,stop.eventId])] = {grant:g.id,eventId:stop.eventId,id,seq:n.seq,attempts:0,due:now,lastOutcome:null};
      }
    }
    const dedupeIds = Object.keys(n.dedupe);
    if (dedupeIds.length >= MAX_DEDUPE) {
      dedupeIds.sort((a,b) => n.dedupe[a].admitted - n.dedupe[b].admitted);
      delete n.dedupe[dedupeIds[0]];
    }
    n.dedupe[stop.eventId] = {admitted:now, grants:eligible.map(g => g.id)};
  }
  return [n,true];
}
export function poll(s: State, input: GrantInput, cursor: string | null, batch: number, now: number, nextCursor: string, maxAgeMs?: number): [State, Result<{events: Stop[]; cursor:string; truncated:boolean; hasMore:boolean; nextPollMs:number}>] {
  const n = expire(s,now); const g = Object.values(n.grants).find(x => !x.reader && key(x) === key(input));
  if (!g) return [n,{ok:false,error:'not_found'}];
  if (!g.authorized) return [n,{ok:false,error:'unavailable'}];
  if (!Number.isInteger(batch) || batch < 1 || (maxAgeMs !== undefined && (!Number.isInteger(maxAgeMs) || maxAgeMs < 0))) return [n,{ok:false,error:'invalid_params'}];
  let c: {grant:string;position:number} | undefined;
  if (cursor !== null) {
    try { c = JSON.parse(atob(cursor)) as {grant:string;position:number}; } catch { /* invalid cursor */ }
  }
  let truncated = cursor !== null && (!c || c.grant !== g.id || !Number.isSafeInteger(c.position) || c.position < 0 || c.position > n.seq);
  let position = cursor === null || truncated ? n.seq : c!.position;
  if (!truncated && position < g.floor) { truncated = true; position = g.floor; }
  // A presented cursor acknowledges everything through its position.
  for (const id of [...g.events]) {
    const row = n.events[id];
    if (row.seq > position) continue;
    g.events = g.events.filter(x => x !== id);
    row.grants = row.grants.filter(x => x !== g.id);
    if (!row.grants.length) delete n.events[id];
  }
  g.floor = Math.max(g.floor, position);
  let advanced = position;
  let entries = g.events.map(id => n.events[id]).filter(e => e && e.seq > position).sort((a,b) => a.seq-b.seq);
  if (cursor !== null && !truncated && maxAgeMs !== undefined) {
    const filtered = entries.filter(e => Date.parse(e.stop.observed_at) >= now - maxAgeMs);
    truncated = filtered.length !== entries.length;
    if (truncated && !filtered.length) advanced = entries[entries.length - 1].seq;
    entries = filtered;
  }
  const selected = entries.slice(0,Math.min(batch,MAX_BATCH));
  const watermark = selected.length ? selected[selected.length-1].seq : advanced;
  const token = btoa(JSON.stringify({grant:g.id,position:watermark}));
  return [n,{ok:true,value:{events:selected.map(e => e.stop),cursor:token,truncated,hasMore:entries.length > selected.length,nextPollMs:1000}}];
}

// The broker adapter commits the admission before sending the node's ACK.
export async function acknowledge(stop: Stop, commit: (stop: Stop) => Promise<unknown>, send: (eventId: string) => void): Promise<void> {
  await commit(stop);
  if (typeof stop.eventId === 'string' && stop.eventId) send(stop.eventId);
}

export function reserve(s:State, now:number): [State, {key:string; message:Message; grant:Grant; stop:Stop; cursor:string} | null] {
  let n = expire(s,now);
  for (const [key,m] of Object.entries(n.messages)) {
    const g = n.grants[m.grant];
    if (!g || !g.authorized || g.deliveryActive === false || m.due > now) continue;
    if (m.attempts >= 10) {
      n = delivered(n,key,m.attempts,{status:'retryable',category:m.lastOutcome ?? 'connection_refused'});
      return [n,null];
    }
    m.attempts++; m.due = now + Math.min(1000 * 2 ** (m.attempts - 1), 60000);
    const unresolved = Object.values(n.messages).filter(x => x.grant === g.id);
    const position = Math.min(...unresolved.map(x => x.seq)) - 1;
    const cursor = btoa(JSON.stringify({grant:g.id,position}));
    return [n,{key,message:structuredClone(m),grant:structuredClone(g),stop:structuredClone(n.events[m.eventId].stop),cursor}];
  }
  return [n,null];
}
export function delivered(s:State, key:string, attempt:number, outcome:DeliveryResult):State {
  const n = structuredClone(s), m = n.messages[key];
  if (!m || m.attempts !== attempt) return n;
  if (outcome.status === 'deferred') { m.attempts--; return n; }
  if (outcome.status === 'accepted' || outcome.status === 'terminal' || (outcome.status === 'retryable' && m.attempts >= 10)) {
    delete n.messages[key];
    const g = n.grants[m.grant], row = n.events[m.eventId];
    if (g && outcome.status !== 'accepted') { if (outcome.status === 'retryable') g.deliveryActive = false; g.lastOutcome = outcome.category; }
    if (g) g.events = g.events.filter(id => id !== m.eventId);
    if (row) { row.grants = row.grants.filter(id => id !== m.grant); if (!row.grants.length) delete n.events[m.eventId]; }
  } else { m.lastOutcome = outcome.category; const g = n.grants[m.grant]; if (g) g.lastOutcome = outcome.category; }
  return n;
}
