// Private per-node limits: 128 grants, 256 retained occurrences per grant,
// 100 events per poll, and 2048 dedupe IDs per node for at most 24 hours.
// A later resend, or one evicted by the cap, can be delivered again with the
// same eventId. Only grants live at the observation time are eligible.
export const MAX_GRANTS = 128;
export const MAX_EVENTS = 256;
export const MAX_BATCH = 100;
export const MAX_DEDUPE = 2048;
export const DEDUPE_MS = 24 * 60 * 60 * 1000;
const LEASE_MS = 300_000;
// Node and Cloudflare clocks are assumed NTP-synchronized. The 30-second
// tolerance favors delivery: an early stop can duplicate the read-once result,
// whereas dropping a real stop loses its notification.
export const CLOCK_SKEW_MS = 30_000;

export type Principal = { issuer: string; subject: string };
export type GrantInput = { principal: Principal; node: string; name: string; arguments: { node: string; session_id: string }; profile: string; mode: 'poll' | 'push' | 'webhook'; callback?: string };
export type Stop = { type: 'turn_stop'; eventId: string; session_id: string; state: 'idle' | 'waiting_on_human' | 'exited' | 'unknown'; observed_at: string };
export type Grant = GrantInput & { id: string; created: number; expires: number; authorized: boolean; events: string[]; floor: number; reader?: true };
type Entry = { stop: Stop; seq: number; grants: string[] };
type Dedupe = { admitted: number; grants: string[] };
export type State = { grants: Record<string, Grant>; events: Record<string, Entry>; dedupe: Record<string, Dedupe>; seq: number };
export type Category = 'unavailable' | 'forbidden' | 'not_found' | 'invalid_params' | 'limit';
export type Result<T> = { ok: true; value: T } | { ok: false; error: Category };
export const empty = (): State => ({ grants: {}, events: {}, dedupe: {}, seq: 0 });
export const key = (g: GrantInput): string => JSON.stringify([g.principal.issuer, g.principal.subject, g.node, g.name, g.arguments.node, g.arguments.session_id, g.mode, g.mode === 'webhook' ? g.callback : null]);
export function valid(g: GrantInput): boolean {
  return !!(g && g.principal && typeof g.principal.issuer === 'string' && g.principal.issuer &&
    typeof g.principal.subject === 'string' && g.principal.subject && typeof g.node === 'string' && g.node &&
    g.name === 'agent.session_state_changed' && g.arguments && typeof g.arguments === 'object' &&
    Object.keys(g.arguments).length === 2 && g.arguments.node === g.node &&
    typeof g.arguments.session_id === 'string' && g.arguments.session_id &&
    typeof g.profile === 'string' && g.profile && ['poll', 'push', 'webhook'].includes(g.mode) &&
    (g.mode !== 'webhook' || (typeof g.callback === 'string' && g.callback)));
}
function remove(s: State, id: string): void {
  const g = s.grants[id]; if (!g) return;
  for (const eventId of g.events) {
    const e = s.events[eventId]; if (!e) continue;
    e.grants = e.grants.filter(x => x !== id);
    if (!e.grants.length) delete s.events[eventId];
  }
  for (const d of Object.values(s.dedupe)) {
    d.grants = d.grants.filter(g => g !== id);
  }
  delete s.grants[id];
}
export function removeReaders(s: State, live: string[]): State {
  const n = structuredClone(s);
  for (const g of Object.values(n.grants)) if (g.reader && !live.includes(g.id)) remove(n, g.id);
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
  for (const [id, d] of Object.entries(n.dedupe)) if (d.admitted + DEDUPE_MS <= now) delete n.dedupe[id];
  return n;
}
export function subscribe(s: State, input: GrantInput, now: number, id: string, decision: 'allowed' | 'denied' | 'unavailable'): [State, Result<Grant>] {
  let n = expire(s, now);
  if (!valid(input)) return [n, {ok:false,error:'invalid_params'}];
  const existing = Object.values(n.grants).find(g => !g.reader && key(g) === key(input));
  if (existing) {
    if (decision === 'denied') { remove(n, existing.id); return [n, {ok:false,error:'forbidden'}]; }
    existing.expires = now + LEASE_MS;
    existing.authorized = decision === 'allowed';
    return [n, {ok:true,value:existing}];
  }
  if (decision !== 'allowed') return [n, {ok:false,error:decision === 'denied' ? 'forbidden' : 'unavailable'}];
  if (Object.keys(n.grants).length >= MAX_GRANTS) return [n, {ok:false,error:'limit'}];
  const g: Grant = { ...structuredClone(input), id, created:now, expires:now + LEASE_MS, authorized:true, events:[], floor:0 };
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
  if (!g) return [n,{ok:false,error:'not_found'}];
  remove(n,g.id); return [n,{ok:true,value:null}];
}
export function admit(s: State, stop: Stop, now: number): [State, boolean] {
  const n = expire(s,now);
  if (!stop.eventId || !stop.session_id || !['idle','waiting_on_human','exited','unknown'].includes(stop.state) || !Number.isFinite(Date.parse(stop.observed_at))) return [n,false];
  if (n.events[stop.eventId] || n.dedupe[stop.eventId]) return [n,true];
  const matching = Object.values(n.grants).filter(g => g.arguments.session_id === stop.session_id && Date.parse(stop.observed_at) >= g.created - CLOCK_SKEW_MS);
  for (const g of matching) {
    if (g.events.length >= MAX_EVENTS) {
      if (g.reader) { remove(n, g.id); continue; }
      const oldest = g.events.shift()!;
      const row = n.events[oldest];
      g.floor = Math.max(g.floor, row.seq);
      row.grants = row.grants.filter(id => id !== g.id);
      if (!row.grants.length) delete n.events[oldest];
    }
  }
  const eligible = matching.filter(g => !!n.grants[g.id]);
  if (eligible.length) {
    n.events[stop.eventId] = { stop, seq: ++n.seq, grants: eligible.map(g => g.id) };
    for (const g of eligible) g.events.push(stop.eventId);
    const ids = Object.keys(n.dedupe);
    if (ids.length >= MAX_DEDUPE) {
      ids.sort((a,b) => n.dedupe[a].admitted - n.dedupe[b].admitted);
      delete n.dedupe[ids[0]];
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
