import { type GrantInput, MAX_EVENTS, type Principal, type Profile, type Result, type Stop, type WebhookOptions } from './events.ts';

const NAME = 'agent.session_state_changed';
const descriptor = {
  name: NAME,
  description: 'A Reach prompt turn stopped, or its observation became unknown. Subscribe, then read the current session once.',
  inputSchema: { type:'object', properties:{node:{type:'string',minLength:1},session_id:{type:'string',minLength:1}}, required:['node','session_id'], additionalProperties:false },
  payloadSchema: { type:'object', properties:{node:{type:'string'},session_id:{type:'string'},state:{type:'string',enum:['idle','waiting_on_human','exited','unknown']},observed_at:{type:'string',format:'date-time'}}, required:['node','session_id','state','observed_at'], additionalProperties:false },
};

type Broker = {
  openReader(input: GrantInput, cursor: string | null): Promise<Result<ReadableStream<Uint8Array>>>;
  subscribe(input: GrantInput, options?: WebhookOptions): Promise<Result<unknown>>;
  unsubscribe(input: GrantInput): Promise<Result<null>>;
  poll(input: GrantInput, cursor: string | null, batch: number, maxAgeMs?: number): Promise<Result<{events:Stop[];cursor:string;truncated:boolean;hasMore:boolean;nextPollMs:number}>>;
};
type ErrorValue = {code:number; message:string; data?:unknown};
const error = (code:number, message:string, data?:unknown): ErrorValue => ({code,message,...(data === undefined ? {} : {data})});
const invalid = () => error(-32602,'Invalid params');
function mapped(category:string, profile:Profile): ErrorValue {
  switch (category) {
    case 'invalid_params': return invalid();
    case 'not_found': return error(-32023,'Not found',{kind:'subscription'});
    case 'forbidden': return error(-32024,'Forbidden');
    case 'limit': return error(-32025,'Resource exhausted',{limit:'subscriptions'});
    case 'unavailable': return error(-32000,'Node unavailable');
    default: return error(profile === 'standard' ? -32027 : -32015,'Callback endpoint error',{reason: category === 'callback_endpoint' || category === 'resolution_failed' ? 'connection_refused' : category});
  }
}
export async function eventsWire(method:string, params:unknown, profile:Profile, principal:Principal, nodes: {has(node:string):boolean}, broker:(node:string)=>Broker): Promise<{result?:unknown;error?:ErrorValue;stream?:ReadableStream<Uint8Array>}> {
  if (method === 'events/list') return {result:{events:[{...descriptor,delivery:profile === 'standard' ? ['poll','push','webhook'] : ['webhook']}]}};
  if (!['events/poll','events/stream','events/subscribe','events/unsubscribe'].includes(method) || ((method === 'events/poll' || method === 'events/stream') && profile === 'chatgpt')) return {error:error(-32601,'Method not found')};
  if (!params || typeof params !== 'object' || Array.isArray(params)) return {error:invalid()};
  const p = params as Record<string, any>;
  if (typeof p.name !== 'string') return {error:invalid()};
  if (p.name !== NAME) return {error:error(-32023,'Not found',{kind:'event'})};
  const args = p.arguments;
  if (!args || typeof args !== 'object' || Array.isArray(args) || Object.keys(args).length !== 2 || typeof args.node !== 'string' || !args.node || typeof args.session_id !== 'string' || !args.session_id) return {error:invalid()};
  const input:GrantInput = {principal,node:args.node,name:NAME,arguments:{node:args.node,session_id:args.session_id},profile,mode:method === 'events/poll' ? 'poll' : method === 'events/stream' ? 'push' : 'webhook'};
  if (method !== 'events/poll' && method !== 'events/stream') {
    if (!p.delivery || typeof p.delivery !== 'object' || typeof p.delivery.url !== 'string') return {error:invalid()};
    if (method === 'events/subscribe' && p.delivery.mode !== 'webhook') return {error:error(-32026,'Unsupported',{feature:'deliveryMode',value:p.delivery.mode})};
    if (method === 'events/unsubscribe' && p.delivery.mode !== undefined && p.delivery.mode !== 'webhook') return {error:error(-32026,'Unsupported',{feature:'deliveryMode',value:p.delivery.mode})};
    try { if (new URL(p.delivery.url).protocol !== 'https:') return {error:invalid()}; } catch { return {error:invalid()}; }
    input.callback = p.delivery.url;
    if (method === 'events/subscribe') input.secret = p.delivery.secret;
  }
  if (!nodes.has(args.node)) return {error:mapped('unavailable',profile)};
  try {
    const target = broker(args.node);
    if (method === 'events/stream') {
      if (!(p.cursor === null || typeof p.cursor === 'string') || (p.maxAgeMs !== undefined && (!Number.isSafeInteger(p.maxAgeMs) || p.maxAgeMs < 0)) || (p.heartbeatMs !== undefined && (!Number.isSafeInteger(p.heartbeatMs) || p.heartbeatMs < 1))) return {error:invalid()};
      const r = await target.openReader(input,p.cursor);
      return r.ok ? {stream:r.value} : {error:mapped(r.error,profile)};
    }
    if (method === 'events/poll') {
      if (!(p.cursor === null || typeof p.cursor === 'string') || (p.maxEvents !== undefined && (!Number.isSafeInteger(p.maxEvents) || p.maxEvents < 1)) || (p.maxAgeMs !== undefined && (!Number.isSafeInteger(p.maxAgeMs) || p.maxAgeMs < 0))) return {error:invalid()};
      const r = await target.poll(input,p.cursor,p.maxEvents ?? 100,p.maxAgeMs);
      if (!r.ok) return {error:mapped(r.error,profile)};
      return {result:{...r.value,events:r.value.events.map(e => occurrence(e,args.node))}};
    }
    const r = method === 'events/subscribe' ? await target.subscribe(input,{ttlMs:p.ttlMs,cursor:p.cursor,maxAgeMs:p.maxAgeMs}) : await target.unsubscribe(input);
    if (!r.ok) {
      if (method === 'events/unsubscribe' && profile === 'chatgpt' && r.error === 'not_found') return {result:{}};
      return {error:mapped(r.error,profile)};
    }
    return {result:method === 'events/unsubscribe' ? {} : r.value};
  } catch { return {error:mapped('unavailable',profile)}; }
}

// One occurrence as callers see it, in poll results and stream notifications:
// only the payload fields, never the node's frame or anything else it carries.
export function occurrence(stop: Stop, node: string) {
  return {eventId:stop.eventId,name:NAME,timestamp:stop.observed_at,data:{node,session_id:stop.session_id,state:stop.state,observed_at:stop.observed_at}};
}

// The stream notification one broker reader record stands for, or null for a
// record the wire does not forward. The cursor, when present, is the
// position the notification carries.
export function readerNotification(record: any, node: string, profile: Profile): {method:'active'|'event'|'terminated'; data:Record<string,unknown>; cursor?:string} | null {
  switch (record?.type) {
    case 'active':
    case 'gap':
      return {method:'active',data:{cursor:record.cursor,truncated:record.type === 'gap' || !!record.truncated},cursor:record.cursor};
    case 'event':
      return {method:'event',data:{...occurrence(record.event,node),cursor:record.cursor},cursor:record.cursor};
    case 'terminated': {
      // A reader ends on overflow of its retained events, not of a subscription quota.
      const ended = record.reason === 'limit' ? error(-32025,'Resource exhausted',{limit:'events',max:MAX_EVENTS})
        : record.reason === 'forbidden' ? mapped('forbidden',profile) : error(-32000,'Subscription ended');
      return {method:'terminated',data:{error:ended}};
    }
  }
  return null;
}
