import * as e from './events.ts';
function assert(v: unknown, message: string): asserts v { if (!v) throw new Error(message); }
const base: e.GrantInput = { principal:{issuer:'issuer',subject:'alice'},node:'node',name:'agent.session_state_changed',arguments:{node:'node',session_id:'session'},profile:'standard',mode:'poll' };
const extraArgs = {...base.arguments,extra:'x'};
assert(!e.valid({...base,arguments:extraArgs}), 'arguments reject additional properties');
let s=e.empty();
let outcome: e.Result<e.Grant>;
[s,outcome]=e.subscribe(s,base,100_000,'grant','unavailable'); assert(!outcome.ok && outcome.error==='unavailable','offline');
[s,outcome]=e.subscribe(s,base,100_000,'grant','denied'); assert(!outcome.ok && outcome.error==='forbidden','denied');
[s,outcome]=e.subscribe(s,base,100_000,'grant','allowed'); assert(outcome.ok,'allowed');
s=e.setAuthorization(s,'grant','unavailable',100_000); assert(s.grants.grant.authorized,'stale unavailable cannot undo a newer allowed result');
let p: e.Result<{events:e.Stop[];cursor:string;truncated:boolean;hasMore:boolean;nextPollMs:number}>;
[s,p]=e.poll(s,base,null,10,100_000,'cursor'); assert(p.ok && p.value.events.length===0,'null cursor');
const initial = p.value.cursor;
const stop=(id:string,time:number):e.Stop=>({type:'turn_stop',eventId:id,session_id:'session',state:'unknown',observed_at:new Date(time).toISOString()});
let ack:boolean;
[s,ack]=e.admit(s,stop('too-old',69_999),100_000); assert(ack && !s.events['too-old'],'skew exclusion');
[s,ack]=e.admit(s,stop('first',70_000),100_000); assert(ack && !!s.events.first,'skew inclusion');
[s,ack]=e.admit(s,stop('first',70_000),100_000); assert(ack && s.grants.grant.events.length===1,'dedup');
[s,ack]=e.admit(s,stop('second',100_000),100_000);
[s,p]=e.poll(s,base,initial,1,100_000,'next'); assert(p.ok && p.value.events[0].eventId==='first' && p.value.hasMore,'ordered batch');
const next = p.value.cursor;
[s,p]=e.poll(s,base,next,1,100_000,'last'); assert(p.ok && p.value.events[0].eventId==='second' && !p.value.hasMore,'next batch');
const other={...base,principal:{issuer:'issuer',subject:'bob'}};
[s,outcome]=e.subscribe(s,other,100_000,'other','allowed');
[s,p]=e.poll(s,other,initial,10,100_000,'foreign'); assert(p.ok && p.value.truncated && !p.value.events.length,'foreign cursor');
[s,ack]=e.admit(s,stop('shared',100_000),100_000); assert(ack && s.events.shared.grants.length===2,'shared event');
[s,outcome]=e.subscribe(s,base,110_000,'ignored','unavailable'); assert(outcome.ok && !outcome.value.authorized && outcome.value.created===100_000,'offline refresh');
s=e.setAuthorization(s,'grant','denied',110_000); assert(!s.grants.grant && !!s.events.shared && s.events.shared.grants.length===1,'denial removes membership');
s=e.expire(s,400_000); assert(!s.grants.other && !s.events.shared,'expiry');
[s,outcome]=e.subscribe(s,base,500_000,'new','allowed');
[s,p]=e.poll(s,base,initial,10,500_000,'expired'); assert(p.ok && p.value.truncated,'expired cursor');
for (let i=0;i<e.MAX_EVENTS;i++) [s,ack]=e.admit(s,stop('event'+i,500_000),500_000);
[s,ack]=e.admit(s,stop('overflow',500_000),500_000); assert(ack && !!s.events.overflow && !s.events.event0,'overflow is admitted');
[s,p]=e.poll(s,base,btoa(JSON.stringify({grant:'new',position:0})),10,500_000,'x'); assert(p.ok && p.value.truncated && p.value.events[0].eventId==='event1','overflow signals truncation');
let cursor = p.value.cursor;
for (let i=0;i<e.MAX_EVENTS+5;i++) {
  [s,ack]=e.admit(s,stop('stream'+i,500_000),500_000);
  [s,p]=e.poll(s,base,cursor,e.MAX_BATCH,500_000,'x'); assert(p.ok,'stream poll'); cursor=p.value.cursor;
}
assert(s.grants.new.events.length < e.MAX_EVENTS,'consumed positions release capacity');
let d=e.empty();
[d,outcome]=e.subscribe(d,base,1_000_000,'d','allowed');
[d,p]=e.poll(d,base,null,1,1_000_000,'x'); assert(p.ok,'initial'); cursor=p.value.cursor;
[d,ack]=e.admit(d,stop('dedupe',1_000_000),1_000_000);
[d,p]=e.poll(d,base,cursor,1,1_000_000,'x'); assert(p.ok,'read'); cursor=p.value.cursor;
[d,p]=e.poll(d,base,cursor,1,1_000_000,'x'); assert(p.ok && !d.events.dedupe,'release read row');
[d,ack]=e.admit(d,stop('dedupe',1_000_000),1_000_001); assert(ack && !d.events.dedupe,'dedupe survives consumption');
// Keep a current grant while advancing past the dedupe window.
d.grants.d.expires = 1_000_000 + e.DEDUPE_MS + 100;
[d,ack]=e.admit(d,stop('dedupe',1_000_000),1_000_000+e.DEDUPE_MS); assert(ack && !!d.events.dedupe,'dedupe window expires');
for (let i=0;i<e.MAX_DEDUPE+1;i++) [d,ack]=e.admit(d,stop('cap'+i,1_000_000),1_000_000+i);
assert(Object.keys(d.dedupe).length===e.MAX_DEDUPE && !d.dedupe.cap0 && !d.dedupe.cap1 && !!d.dedupe.dedupe,'dedupe count evicts oldest by admission time');
let limit=e.empty();
for (let i=0;i<e.MAX_GRANTS;i++) [limit,outcome]=e.subscribe(limit,{...base,principal:{issuer:'issuer',subject:'p'+i}},1_000_000,'g'+i,'allowed');
[limit,outcome]=e.subscribe(limit,{...base,principal:{issuer:'issuer',subject:'extra'}},1_000_000,'extra','allowed'); assert(!outcome.ok && outcome.error==='limit','subscription limit');

let cstate=e.empty();
[cstate,outcome]=e.subscribe(cstate,base,2_000_000,'alice-c','allowed');
[cstate,p]=e.poll(cstate,base,null,10,2_000_000,'x'); assert(p.ok,'alice starts'); const aliceCursor=p.value.cursor;
[cstate,outcome]=e.subscribe(cstate,other,2_000_000,'bob-c','allowed');
[cstate,ack]=e.admit(cstate,stop('unread',2_000_000),2_000_000);
for (let i=0;i<1025;i++) [cstate,p]=e.poll(cstate,other,null,10,2_000_000,'x');
[cstate,p]=e.poll(cstate,base,aliceCursor,10,2_000_000,'x'); assert(p.ok && !p.value.truncated && p.value.events[0].eventId==='unread','other principal cannot evict cursor');
let b=e.empty();
[b,outcome]=e.subscribe(b,base,3_000_000,'batch','allowed');
[b,p]=e.poll(b,base,null,1,3_000_000,'x'); assert(p.ok,'batch start'); const batchCursor=p.value.cursor;
for (let i=0;i<e.MAX_BATCH+1;i++) [b,ack]=e.admit(b,stop('batch'+i,3_000_000),3_000_000);
[b,p]=e.poll(b,base,batchCursor,e.MAX_BATCH+10,3_000_000,'x'); assert(p.ok && p.value.events.length===e.MAX_BATCH && p.value.hasMore,'batch limit');
const effects:string[]=[];
await e.acknowledge(stop('committed',1_000_000), async () => {effects.push('commit');}, id => effects.push('ack:'+id));
assert(effects.join(',')==='commit,ack:committed','ack after commit');
try { await e.acknowledge(stop('failed',1_000_000), async () => {throw new Error('storage');}, id => effects.push('ack:'+id)); } catch { /* expected */ }
assert(!effects.includes('ack:failed'),'storage failure does not ack');
console.log('events tests passed');

// Exercise real SQLite-backed DO storage and its daemon WebSocket locally.
const { createTestHarness } = await (Function("return import('wrangler')")() as Promise<{createTestHarness: (options: unknown) => any}>);
const harness = createTestHarness({workers:[{configPath:'./wrangler.jsonc'}]});
try {
  await harness.listen();
  const worker = harness.getWorker();
  let env = await worker.getEnv();
  let stub = env.NODE.get(env.NODE.idFromName('node'));
  const response = await stub.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  assert(response.status === 101 && response.webSocket, 'real DO socket');
  let socket = response.webSocket;
  let receivedAck: ((id:string) => void) | undefined;
  let holdAuthorization = false;
  const authorizationRequests: Array<{id:string}> = [];
  let authorizationWaiter: (() => void) | undefined;
  const nextAuthorization = async () => {
    if (!authorizationRequests.length) await new Promise<void>((resolve,reject) => { const timer=setTimeout(() => reject(new Error('authorization timeout')),5000); authorizationWaiter = () => {clearTimeout(timer); resolve();}; });
    return authorizationRequests.shift()!;
  };
  const connectHandler = () => { socket.accept(); socket.addEventListener('message', (event: {data:string}) => {
    const frame = JSON.parse(event.data);
    if (frame.method === 'events.authorize') {
      if (holdAuthorization) { authorizationRequests.push(frame); authorizationWaiter?.(); authorizationWaiter=undefined; }
      else socket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));
    }
    if (frame.type === 'turn_stop_ack') receivedAck?.(frame.eventId);
  }); };
  connectHandler();
  const granted = await stub.subscribe(base);
  assert(granted.ok, 'real DO grant');
  const start = await stub.poll(base,null,10);
  assert(start.ok, 'real DO initial poll');
  const startCursor = start.value.cursor;
  const event = stop('durable-event',Date.now());
  const send = () => new Promise<string>((resolve,reject) => { const timer=setTimeout(() => reject(new Error('ack timeout')),5000); receivedAck = id => {clearTimeout(timer); resolve(id);}; socket.send(JSON.stringify(event)); });
  // The test observes the ACK at the transport but deliberately leaves the
  // simulated node outbox unacknowledged, as if that ACK had been lost.
  assert(await send() === event.eventId, 'real DO acknowledges');
  const firstRead = await stub.poll(base,startCursor,10);
  assert(firstRead.ok && firstRead.value.events.length === 1 && firstRead.value.events[0].eventId === event.eventId, 'acknowledged record committed');
  socket.close();
  await harness.update({workers:[{configPath:'./wrangler.jsonc'}]});
  env = await worker.getEnv();
  stub = env.NODE.get(env.NODE.idFromName('node'));
  const renewed = await stub.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  assert(renewed.webSocket, 'reconnected after worker reload');
  socket = renewed.webSocket;
  connectHandler();
  assert(await send() === event.eventId, 'real DO duplicate acknowledgement after restart');
  const reread = await stub.poll(base,startCursor,10);
  assert(reread.ok && reread.value.events.length === 1, 'duplicate has one record');
  holdAuthorization = true;
  const replacement = await stub.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  assert(replacement.webSocket, 'replacement connection');
  socket = replacement.webSocket;
  connectHandler();
  const reconnectRequest = await nextAuthorization();
  const pendingRead = stub.poll(base,startCursor,10);
  const pollRequest = await nextAuthorization();
  const early = await Promise.race([pendingRead.then(() => true),new Promise<boolean>(resolve => setTimeout(()=>resolve(false),30))]);
  assert(!early, 'poll cannot deliver before its node authorization');
  socket.send(JSON.stringify({id:reconnectRequest.id,result:{decision:'unavailable'}}));
  await new Promise(resolve => setTimeout(resolve,10));
  socket.send(JSON.stringify({id:pollRequest.id,result:{decision:'allowed'}}));
  holdAuthorization = false;
  const resumed = await pendingRead;
  assert(resumed.ok && resumed.value.events.length===1, 'poll retries transient unavailable and resumes delivery');
  socket.close();
} finally {
  await harness.close();
}
console.log('real DO storage test passed');

// A test-only subclass exposes the actual storage alarm and lets the test put
// a persisted deadline in the past before invoking the production alarm.
const { build } = await (Function("return import('esbuild')")() as Promise<{build:(options:unknown)=>Promise<any>}>);
const fs = await (Function("return import('node:fs/promises')")() as Promise<any>);
const bundled = await build({stdin:{contents:`
import { NodeBroker } from './src/broker.ts';
export class TestBroker extends NodeBroker {
  async inspectAlarm() { return this.ctx.storage.getAlarm(); }
  async expireAndFire(id) {
    const key = 'grant/' + id;
    const grant = await this.ctx.storage.get(key);
    grant.expires = Date.now() - 1;
    await this.ctx.storage.put(key, grant);
    await this.alarm();
  }
  async hasGrant(id) { return !!(await this.ctx.storage.get('grant/' + id)); }
  async inspectEvent(id) { return this.ctx.storage.get('event/' + id); }
  async inspectGrant(id) { return this.ctx.storage.get('grant/' + id); }
  async shortenGrant(id) {
    const key = 'grant/' + id;
    const grant = await this.ctx.storage.get(key);
    grant.expires = Date.now() + 10000;
    await this.ctx.storage.put(key, grant);
    return grant.expires;
  }
}
export default { fetch() { return new Response('ok'); } };
`,resolveDir:Function('return process.cwd()')(),sourcefile:'alarm-fixture.ts'},bundle:true,write:false,format:'esm',platform:'browser',external:['cloudflare:workers']});
await fs.mkdir('.wrangler/tmp',{recursive:true});
await fs.writeFile('.wrangler/tmp/alarm-fixture.mjs',bundled.outputFiles[0].text);
const alarmHarness = createTestHarness({workers:[{config:{name:'alarm-test',main:'.wrangler/tmp/alarm-fixture.mjs',compatibility_date:'2026-11-01',durable_objects:{bindings:[{name:'NODE',class_name:'TestBroker'}]},migrations:[{tag:'v1',new_sqlite_classes:['TestBroker']}]}}]});
try {
  await alarmHarness.listen();
  const alarmEnv = await alarmHarness.getWorker().getEnv();
  const ns = alarmEnv.NODE;
  const object = ns.get(ns.idFromName('alarm-node'));
  const response = await object.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  const socket = response.webSocket;
  assert(socket,'alarm fixture socket');
  socket.accept();
  let finishAlarmAck: (()=>void) | undefined;
  socket.addEventListener('message',(message:{data:string}) => {
    const frame = JSON.parse(message.data);
    if (frame.method === 'events.authorize') socket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));
    if (frame.type === 'turn_stop_ack') finishAlarmAck?.();
  });
  const first = await object.subscribe(base);
  assert(first.ok, 'alarm fixture first grant');
  await new Promise(resolve => setTimeout(resolve,10));
  const second = await object.subscribe(other);
  assert(second.ok, 'alarm fixture second grant');
  assert(await object.inspectAlarm() === first.value.expires, 'earliest persisted alarm');
  await new Promise<void>(resolve => {finishAlarmAck=resolve; socket.send(JSON.stringify(stop('alarm-shared',Date.now())));});
  const beforeAlarm = await object.inspectEvent('alarm-shared');
  assert(beforeAlarm.grants.length===2, 'event is shared before expiry');
  await object.expireAndFire(first.value.id);
  assert(!(await object.hasGrant(first.value.id)) && await object.hasGrant(second.value.id), 'alarm deletes only expired grant');
  const afterAlarm = await object.inspectEvent('alarm-shared');
  assert(afterAlarm.grants.length===1 && afterAlarm.grants[0]===second.value.id, 'alarm removes expired membership and preserves shared row');
  assert(await object.inspectAlarm() === second.value.expires, 'alarm rescheduled to next deadline');
  const shortDeadline = await object.shortenGrant(second.value.id);
  const closed = new Promise<void>(resolve => socket.addEventListener('close',()=>resolve()));
  socket.close();
  await closed;
  const offlinePoll = await object.poll(other,null,10);
  assert(!offlinePoll.ok && offlinePoll.error==='unavailable', 'offline poll reports unavailable');
  const refreshed = await object.inspectGrant(second.value.id);
  assert(refreshed.expires > shortDeadline + 200_000 && !refreshed.authorized, 'offline poll extends grant and awaits reauthorization');
  assert(!!(await object.inspectEvent('alarm-shared')), 'offline refresh retains acknowledged event');
  const replacement = await object.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  const socket2 = replacement.webSocket;
  assert(socket2,'denial reconnect socket');
  socket2.accept();
  const reauthorization = await new Promise<string>((resolve,reject) => {
    const timer = setTimeout(() => reject(new Error('denial reauthorization timeout')),5000);
    socket2.addEventListener('message',(message:{data:string}) => {
      const frame = JSON.parse(message.data);
      if (frame.method === 'events.authorize') { clearTimeout(timer); resolve(frame.id); }
    });
  });
  socket2.send(JSON.stringify({id:reauthorization,result:{decision:'denied'}}));
  for (let i=0;i<50 && await object.hasGrant(second.value.id);i++) await new Promise(resolve => setTimeout(resolve,10));
  assert(!(await object.hasGrant(second.value.id)), 'reconnect denial deletes grant');
  assert(!(await object.inspectEvent('alarm-shared')), 'denial deletes last event membership');
  socket2.close();
} finally {
  await alarmHarness.close();
}
console.log('real DO alarm test passed');
