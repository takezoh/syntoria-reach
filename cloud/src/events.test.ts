import * as e from './events.ts';
function assert(v: unknown, message: string): asserts v { if (!v) throw new Error(message); }
const base: e.GrantInput = { principal:{issuer:'issuer',subject:'alice'},node:'node',name:'agent.session_state_changed',arguments:{node:'node',session_id:'session'},profile:'standard',mode:'poll' };
const extraArgs = {...base.arguments,extra:'x'};
assert(!e.valid({...base,arguments:extraArgs}), 'arguments reject additional properties');
assert(!e.valid({...base,profile:'ChatGPT' as e.Profile}), 'an unknown profile is rejected');
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
{
  const hookA = {...base,mode:'webhook' as const,callback:'https://example.test/hook',secret:'whsec_'+btoa('a'.repeat(32))};
  const hookB = {...hookA,arguments:{node:'node',session_id:'other-session'}};
  let w = e.empty();
  w.pending[e.verificationKey(hookB)] = {id:'pending-b',key:e.key(hookB),nonce:'n',secret:hookB.secret,expires:10_000};
  [w] = e.unsubscribe(w,hookA,1_000);
  assert(!!w.pending[e.verificationKey(hookB)],'unsubscribing another session keeps its challenge');
  [w] = e.unsubscribe(w,hookB,1_000);
  assert(!w.pending[e.verificationKey(hookB)],'unsubscribing the same subscription cancels its challenge');
}
// Delivery state cases use supplied identifiers and time, so retries and crashes are deterministic.
{
  const hook = {...base,mode:'webhook' as const,callback:'https://example.test/hook',secret:'whsec_'+btoa('a'.repeat(32))};
  const chat = {...hook,profile:'chatgpt' as const};
  let state=e.empty();
  [state]=e.subscribe(state,hook,1000,'standard','allowed');
  [state]=e.subscribe(state,chat,1000,'chat','allowed');
  assert(e.key(hook)!==e.key(chat) && Object.keys(state.grants).length===2,'profiles have independent grants');
  [state]=e.subscribe(state,chat,1001,'ignored','allowed');
  assert(state.grants.standard.profile==='standard' && state.grants.chat.profile==='chatgpt','refresh preserves profile');
  [state]=e.admit(state,stop('early',1002),1002,{standard:'random_standard',chat:'unused'});
  assert(Object.values(state.messages).find(m=>m.grant==='chat')?.id==='early','ChatGPT id is eventId');
  let reserved:ReturnType<typeof e.reserve>[1];
  [state,reserved]=e.reserve(state,1002); assert(reserved && reserved.message.attempts===1,'first reservation');
  const first=reserved; const snapshot=structuredClone(state);
  [state,reserved]=e.reserve(snapshot,2002); assert(reserved && reserved.message.id===first.message.id && reserved.message.attempts===2,'lost request preserves reservation and id');
  state=e.delivered(state,reserved.key,2,{status:'terminal',category:'http_4xx'});
  [state]=e.subscribe(state,hook,200_000,'ignored','allowed');
  [state]=e.admit(state,stop('later',300_002),300_002,{standard:'later_standard',chat:'unused'});
  [state,reserved]=e.reserve(state,300_002);
  assert(!reserved || reserved.message.id!=='random_standard','terminal message cannot revive on refresh');
  let only=e.empty(); [only]=e.subscribe(only,hook,1000,'one','allowed');
  [only]=e.admit(only,stop('failed',1001),1001,{one:'failed-id'});
  [only,reserved]=e.reserve(only,1001); assert(reserved,'early reserve');
  only=e.delivered(only,reserved.key,1,{status:'retryable',category:'http_5xx'});
  [only]=e.admit(only,stop('success',1002),1002,{one:'success-id'});
  [only,reserved]=e.reserve(only,1002); assert(reserved && reserved.message.eventId==='success','later message selected');
  assert(JSON.parse(atob(reserved.cursor)).position===0,'later delivery cursor remains before failed event');
  only=e.delivered(only,reserved.key,1,{status:'accepted'});
  let time=2001;
  for (let attempt=2;attempt<=10;attempt++) {
    [only,reserved]=e.reserve(only,time); assert(reserved && reserved.message.attempts===attempt,'bounded attempt '+attempt);
    only=e.delivered(only,reserved.key,attempt,{status:'retryable',category:'timeout'}); time=reserved.message.due;
  }
  assert(!Object.keys(only.messages).length && !only.grants.one.events.length && only.grants.one.lastOutcome==='timeout' && only.grants.one.deliveryActive===false,'exhaustion removes the message and records delivery failure');
  const status=e.webhookResult(only,only.grants.one);
  assert(status.deliveryStatus.lastError==='timeout' && !status.deliveryStatus.active && JSON.parse(atob(status.cursor)).position===only.seq,'exhaustion releases the watermark');
  [only,reserved]=e.reserve(only,time); assert(!reserved,'no eleventh attempt');
  [only,reserved]=e.reserve(only,301000); assert(!reserved && !Object.keys(only.messages).length,'expiry removes messages');
  let cap=e.empty(); [cap]=e.subscribe(cap,hook,1000,'cap-hook','allowed');
  for (let n=0;n<=e.MAX_EVENTS;n++) [cap]=e.admit(cap,stop('cap-hook-'+n,1001),1001,{'cap-hook':'id'+n});
  const gap=Object.values(cap.controls)[0];
  assert(cap.grants['cap-hook'].truncated && gap.body.type==='gap' && gap.id.startsWith('msg_gap_'),'standard eviction queues gap and marks truncation');
  assert(e.webhookResult(cap,cap.grants['cap-hook']).truncated,'refresh projection reports eviction');
  [cap,reserved]=e.reserve(cap,1001); assert(reserved && reserved.message.eventId==='cap-hook-1','eviction cannot leave orphan message');
  let terminal=e.empty(); [terminal]=e.subscribe(terminal,hook,1000,'terminal','allowed');
  [terminal]=e.admit(terminal,stop('gone',1001),1001,{terminal:'msg-gone'});
  [terminal]=e.admit(terminal,stop('after-gone',1001),1001,{terminal:'msg-after'});
  [terminal,reserved]=e.reserve(terminal,1001); assert(reserved,'terminal reserve');
  terminal=e.delivered(terminal,reserved.key,1,{status:'terminal',category:'http_4xx'});
  const terminalStatus=e.webhookResult(terminal,terminal.grants.terminal);
  assert(!terminal.messages[JSON.stringify(['terminal','gone'])] && terminalStatus.deliveryStatus.lastError==='http_4xx' && terminalStatus.deliveryStatus.active && JSON.parse(atob(terminalStatus.cursor)).position===1,'terminal failure releases cursor and reports status');
  [terminal,reserved]=e.reserve(terminal,1001);
  assert(reserved?.message.eventId==='after-gone','terminal failure does not suspend later events');
  assert(e.publicError('callback_endpoint')==='connection_refused' && e.publicError('resolution_failed')==='connection_refused' && e.publicError('http_other')==='http_4xx','internal errors map to public categories');
  terminal=e.deny(terminal,'terminal',1002,'msg_terminated_random');
  assert(!terminal.grants.terminal && terminal.controls.msg_terminated_random.body.type==='terminated','denial retains only termination control');
  let chatOnly=e.empty(); [chatOnly]=e.subscribe(chatOnly,chat,1000,'chat-only','allowed');
  chatOnly=e.deny(chatOnly,'chat-only',1001,'msg_terminated_chat');
  assert(!Object.keys(chatOnly.controls).length,'ChatGPT denial has no control envelope');
}
{
  let state=e.empty(); let grant:e.Result<e.Grant>; let batch:e.Result<{events:e.Stop[];cursor:string;truncated:boolean;hasMore:boolean;nextPollMs:number}>;
  [state,grant]=e.subscribe(state,base,1000,'same-id','allowed');
  [state,batch]=e.poll(state,base,null,10,1000,'unused'); assert(batch.ok,'initial cursor'); const old=batch.value.cursor;
  [state]=e.admit(state,stop('expired-history',1001),1001);
  [state,grant]=e.subscribe(state,base,400000,'same-id','allowed');
  [state,batch]=e.poll(state,base,old,10,400000,'unused');
  assert(batch.ok && batch.value.truncated,'expired cursor signals lost history when identity is reused');
  const hook={...base,mode:'webhook' as const,callback:'https://example.test/hook',secret:'whsec_'+btoa('a'.repeat(32))};
  let h=e.empty(); [h]=e.subscribe(h,hook,1000,'hook','allowed'); [h]=e.admit(h,stop('paused',1001),1001,{hook:'message'});
  let reserved:ReturnType<typeof e.reserve>[1]; [h,reserved]=e.reserve(h,1001); assert(reserved,'reserved before pause');
  h=e.delivered(h,reserved.key,1,{status:'deferred'});
  const msg=Object.values(h.messages)[0];
  assert(!!msg && msg.attempts===0 && msg.lastOutcome===null,'pre-send suspension does not consume or invent an outcome');
}
console.log('events tests passed');

const push = {...base,mode:'push' as const};
let ps=e.empty();
let opened: e.Result<{grant:e.Grant;cursor:string;truncated:boolean}>;
[ps,opened]=e.openReader(ps,push,10_000,'reader1','allowed',null); assert(opened.ok,'first reader'); const oldCursor=opened.value.cursor;
[ps,opened]=e.openReader(ps,push,10_000,'reader2','allowed',null); assert(opened.ok,'independent reader');
[ps,ack]=e.admit(ps,stop('push-shared',10_001),10_001);
assert(ps.events['push-shared'].grants.length===2,'push readers share occurrence');
ps=e.closeReader(ps,'reader1');
assert(!ps.grants.reader1 && ps.events['push-shared'].grants.join() === 'reader2' && ps.dedupe['push-shared'].grants.join() === 'reader2','cancel removes only its membership');
[ps,opened]=e.openReader(ps,push,10_002,'reader3','allowed',oldCursor);
assert(opened.ok && opened.value.truncated && opened.value.cursor!==oldCursor && !ps.grants.reader3.events.length,'old cursor cannot recover');
let read: e.Result<{event:e.Stop;cursor:string}|null>;
[ps,read]=e.readReader(ps,'reader2',10_002); assert(read.ok && read.value?.event.eventId==='push-shared','other reader receives shared occurrence');
ps=e.removeReaders(ps); assert(!Object.keys(ps.grants).length && ps.dedupe['push-shared'].grants.length===0,'restart removes readers and membership');
let solo=e.empty();
[solo,opened]=e.openReader(solo,push,20_000,'solo','allowed',null); assert(opened.ok,'solo reader');
[solo,ack]=e.admit(solo,stop('resend',20_001),20_001); assert(ack,'first admission');
solo=e.closeReader(solo,'solo');
assert(!solo.events.resend && solo.dedupe.resend.grants.length===0,'only admission marker remains');
[solo,opened]=e.openReader(solo,push,20_002,'replacement','allowed',null); assert(opened.ok,'replacement');
[solo,ack]=e.admit(solo,stop('resend',20_001),20_003);
assert(ack && !solo.events.resend && !solo.grants.replacement.events.length,'resend acknowledged without delivery');
[solo,ack]=e.admit(solo,stop('new-stop',20_001),20_003);
assert(ack && solo.grants.replacement.events[0]==='new-stop','new identity retains clock skew tolerance');

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
      assert(/^[0-9a-f]{64}$/.test(frame.params.subscription_key), 'node receives an opaque subscription key');
      if (holdAuthorization) { authorizationRequests.push(frame); authorizationWaiter?.(); authorizationWaiter=undefined; }
      else socket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));
    }
    if (frame.type === 'turn_stop_ack') receivedAck?.(frame.eventId);
  }); };
  connectHandler();
  const pushOne = await stub.openReader(push,null);
  const pushTwo = await stub.openReader(push,null);
  assert(pushOne.ok && pushTwo.ok, 'real DO opens independent readers');
  const readerOne = pushOne.value.getReader(); const readerTwo = pushTwo.value.getReader();
  const decode = (chunk: Uint8Array) => JSON.parse(new TextDecoder().decode(chunk));
  const activeOne = decode((await readerOne.read()).value); const activeTwo = decode((await readerTwo.read()).value);
  assert(activeOne.type === 'active' && !activeOne.truncated, 'initial active');
  await new Promise<void>(resolve => { receivedAck=()=>resolve(); socket.send(JSON.stringify(stop('push-do',Date.now()+1))); });
  assert((await stub.closeReader(push,activeOne.id)).ok,'wire closes cancelled reader');
  await readerOne.cancel();
  const delivered = decode((await readerTwo.read()).value);
  assert(delivered.type === 'event' && delivered.event.eventId === 'push-do', 'other reader receives shared event');
  holdAuthorization = true;
  const pushReconnect = await stub.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  assert(pushReconnect.webSocket, 'push reconnect');
  socket = pushReconnect.webSocket; connectHandler();
  const pushAuthorization = await nextAuthorization();
  for (const id of ['queued-a','queued-b']) {
    await new Promise<void>(resolve => { receivedAck=()=>resolve(); socket.send(JSON.stringify(stop(id,Date.now()))); });
  }
  socket.send(JSON.stringify({id:pushAuthorization.id,result:{decision:'allowed'}}));
  holdAuthorization = false;
  for (const id of ['queued-a','queued-b']) {
    const record = await Promise.race([readerTwo.read(),new Promise<never>((_,reject)=>setTimeout(()=>reject(new Error('queued push timeout')),3000))]);
    const item = decode(record.value);
    assert(item.type === 'event' && item.event.eventId === id, 'push drains queued events after one wake');
  }
  const reopened = await stub.openReader(push,activeOne.cursor);
  assert(reopened.ok, 'replacement reader');
  const replacementReader = reopened.value.getReader();
  const replacementActive = decode((await replacementReader.read()).value);
  assert(replacementActive.truncated && replacementActive.cursor !== activeOne.cursor, 'replacement has fresh truncated cursor');
  await stub.closeReader(push,replacementActive.id); await replacementReader.cancel();
  await stub.closeReader(push,activeTwo.id); await readerTwo.cancel();
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
import { safeHttps } from './src/webhook.ts';
export class TestBroker extends NodeBroker {
  changes = 0;
  change(...args) { this.changes++; return super.change(...args); }
  changeCount() { return this.changes; }
  failChallenge = false;
  holdChallenge = false;
  releaseChallenge() { this.resumeChallenge?.(); this.holdChallenge=false; }
  async armChallenge() { this.holdChallenge=true; }
  async pendingCount() { return (await this.ctx.storage.list({prefix:'verification/'})).size; }
  webhookPort() { const port = safeHttps('https://example.test', async () => ({a:['8.8.8.8'],aaaa:[]}), async (_url,init) => {
    if (this.holdChallenge) await new Promise(resolve => { this.resumeChallenge=resolve; });
    const body=JSON.parse(init.body);
    if (body.type === 'verification') { await this.ctx.storage.put('challenge-count',(await this.ctx.storage.get('challenge-count') ?? 0)+1); return new Response(JSON.stringify({challenge:this.failChallenge ? 'wrong' : body.challenge})); }
    const messages=[...(await this.ctx.storage.list({prefix:'message/'})).values()];
    const headers=Object.fromEntries(new Headers(init.headers));
    const log=await this.ctx.storage.get('test-log') ?? [];
    log.push({body:init.body,headers,messages}); await this.ctx.storage.put('test-log',log);
    return new Response(null,{status:await this.ctx.storage.get('test-status') ?? 503});
  }); return {...port, deliver:async (...args) => {
    if (this.deliveryFault === 'before') { this.deliveryFault = undefined; throw new Error('lost before request'); }
    const outcome = await port.deliver(...args);
    if (this.deliveryFault === 'after') { this.deliveryFault = undefined; throw new Error('lost after acceptance'); }
    return outcome;
  }}; }
  async setFault(value) { this.deliveryFault=value; }
  async setChallengeFailure(value) { this.failChallenge = value; }
  async setDeliveryStatus(value) { await this.ctx.storage.put('test-status',value); }
  async deliveryLog() { return await this.ctx.storage.get('test-log') ?? []; }
  async challengeCount() { return await this.ctx.storage.get('challenge-count') ?? 0; }
  async forceDue() { for (const [key,value] of await this.ctx.storage.list({prefix:'message/'})) { value.due=Date.now()-1; await this.ctx.storage.put(key,value); } }
  async dispatch() { await this.alarm(); }
  async messages() { return [...(await this.ctx.storage.list({prefix:'message/'})).values()]; }
  async controls() { return [...(await this.ctx.storage.list({prefix:'control/'})).values()]; }
  async injectStop(stop) { await this.webSocketMessage({send() {}}, JSON.stringify(stop)); }
  async overflowReader(id, stop) {
    const grant = await this.ctx.storage.get('grant/' + id);
    for (let i=0;i<256;i++) {
      const eventId='overflow-fixture-'+i;
      grant.events.push(eventId);
      await this.ctx.storage.put('event/'+eventId,{stop:{...stop,eventId},seq:i+1,grants:[id]});
    }
    await this.ctx.storage.put('grant/'+id,grant);
    await this.injectStop(stop);
  }
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
  async inspectDedupe(id) { return this.ctx.storage.get('dedupe/' + id); }
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
`,resolveDir:Function('return process.cwd()')(),sourcefile:'alarm-fixture.ts'},bundle:true,write:false,format:'esm',platform:'browser',external:['cloudflare:workers','node:dns/promises']});
await fs.mkdir('.wrangler/tmp',{recursive:true});
await fs.writeFile('.wrangler/tmp/alarm-fixture.mjs',bundled.outputFiles[0].text);
const alarmHarness = createTestHarness({workers:[{config:{name:'alarm-test',main:'.wrangler/tmp/alarm-fixture.mjs',compatibility_date:'2026-11-01',compatibility_flags:['nodejs_compat'],vars:{WEBHOOK_ALLOWED_ORIGINS:'https://example.test'},durable_objects:{bindings:[{name:'NODE',class_name:'TestBroker'}]},migrations:[{tag:'v1',new_sqlite_classes:['TestBroker']}]}}]});
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
  let authorizedExpiry = "";
  socket.addEventListener('message',(message:{data:string}) => {
    const frame = JSON.parse(message.data);
    if (frame.method === 'events.authorize') { authorizedExpiry = frame.params.expires_at; socket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}})); }
    if (frame.type === 'turn_stop_ack') finishAlarmAck?.();
  });
  const idlePush=await object.openReader(push,null); assert(idlePush.ok,'idle reader opens');
  const idleReader=idlePush.value.getReader();
  const idleActive=JSON.parse(new TextDecoder().decode((await idleReader.read()).value));
  const idleFirst=JSON.parse(new TextDecoder().decode((await idleReader.read()).value));
  assert(idleFirst.type==='keepalive','idle reader sends keepalive');
  const idleChanges=await object.changeCount();
  for (let i=0;i<3;i++) assert(JSON.parse(new TextDecoder().decode((await idleReader.read()).value)).type==='keepalive','idle heartbeat');
  assert(await object.changeCount()===idleChanges,'keepalives do not read durable state');
  await object.injectStop(stop('idle-wake',Date.now()));
  const idleEvent=JSON.parse(new TextDecoder().decode((await idleReader.read()).value));
  assert(idleEvent.type==='event' && idleEvent.event.eventId==='idle-wake','real event wakes idle reader');
  await object.closeReader(push,idleActive.id); await idleReader.cancel();
  const webhook = {...base,mode:'webhook' as const,callback:'https://example.test/hook',secret:'whsec_'+btoa('a'.repeat(32))};
  await object.armChallenge();
  const inFlightWebhook = object.subscribe(webhook);
  for (let i=0;i<100 && await object.pendingCount()===0;i++) await new Promise(resolve=>setTimeout(resolve,10));
  assert(await object.pendingCount()===1,'pending challenge is durable');
  const overlappingWebhook = await object.subscribe(webhook);
  assert(!overlappingWebhook.ok && overlappingWebhook.error==='unavailable','only one challenge per principal and URL');
  await object.unsubscribe(webhook);
  await object.releaseChallenge();
  assert(!(await inFlightWebhook).ok,'unsubscribe invalidates an in-flight challenge');
  assert(await object.pendingCount()===0,'single-use challenge removed');
  await object.setChallengeFailure(true);
  const rejectedWebhook = await object.subscribe(webhook);
  assert(!rejectedWebhook.ok && rejectedWebhook.error === 'challenge_failed', 'failed challenge does not activate');
  await object.setChallengeFailure(false);
  const subscribed = await object.subscribe(webhook);
  assert(subscribed.ok && !JSON.stringify(subscribed).includes(webhook.secret), 'webhook subscribe hides secret');
  const bounded = await object.subscribe({...webhook,arguments:{session_id:'session',node:'node'}},{ttlMs:1,cursor:subscribed.value.cursor,maxAgeMs:0});
  assert(bounded.ok && bounded.value.id===subscribed.value.id && !bounded.value.truncated,'canonical refresh and live cursor');
  assert(bounded.value.refreshBefore===authorizedExpiry && (await object.inspectGrant(bounded.value.id)).expires===Date.parse(authorizedExpiry),'authorization, grant and result share deadline');
  assert(Date.parse(bounded.value.refreshBefore)-Date.now()>59000 && Date.parse(bounded.value.refreshBefore)-Date.now()<=60000,'minimum grant');
  const invalidAge = await object.subscribe(webhook,{maxAgeMs:-1});
  assert(!invalidAge.ok && invalidAge.error==='invalid_params','invalid max age rejected');
  const foreign = await object.subscribe(webhook,{cursor:'foreign'});
  assert(foreign.ok && foreign.value.truncated,'foreign cursor truncated');

  const challengeCount = await object.challengeCount();
  const refreshedWebhook = await object.refresh(webhook);
  assert(refreshedWebhook.ok && !JSON.stringify(refreshedWebhook).includes(webhook.secret), 'webhook refresh hides secret');
  const secondWebhook = {...webhook,arguments:{...webhook.arguments,session_id:'other-session'}};
  assert((await object.subscribe(secondWebhook)).ok && await object.challengeCount()===challengeCount,'live proof is shared across refresh and arguments');
  await object.unsubscribe(secondWebhook);
  assert((await object.inspectGrant(subscribed.value.id)).secret === webhook.secret, 'webhook grant retains secret');
  const beforeUnsubscribeLog=(await object.deliveryLog()).length;
  assert((await object.unsubscribe(webhook)).ok, 'webhook fixture cleanup');
  await object.dispatch();
  assert(!(await object.controls()).length && (await object.deliveryLog()).length===beforeUnsubscribeLog,'unsubscribe sends no termination');
  assert((await object.subscribe(webhook)).ok && await object.challengeCount()===challengeCount+1,'last unsubscribe clears proof');
  await object.unsubscribe(webhook);
  const expiringHook=await object.subscribe(webhook); assert(expiringHook.ok,'expiring webhook opens');
  const beforeExpiryLog=(await object.deliveryLog()).length;
  await object.expireAndFire(expiringHook.value.id);
  assert(!(await object.controls()).length && (await object.deliveryLog()).length===beforeExpiryLog,'expiry sends no termination');
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

  const connected = await object.fetch('http://node/', {headers:{Upgrade:'websocket'}});
  const socket3 = connected.webSocket; assert(socket3,'push authorization socket'); socket3.accept();
  socket3.addEventListener('message',(message:{data:string}) => {
    const frame=JSON.parse(message.data);
    if (frame.method==='events.authorize') socket3.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));
  });
  const pushResult=await object.openReader(push,null); assert(pushResult.ok,'push opens');
  const otherResult=await object.openReader(push,null); assert(otherResult.ok,'second push opens');
  const pushReader=pushResult.value.getReader();
  const otherReader=otherResult.value.getReader(); const otherActive=JSON.parse(new TextDecoder().decode((await otherReader.read()).value));
  const active=JSON.parse(new TextDecoder().decode((await pushReader.read()).value));
  const pushId=JSON.parse(atob(active.cursor)).grant;
  const pushClosed=new Promise<void>(resolve=>socket3.addEventListener('close',()=>resolve()));
  socket3.close(); await pushClosed;
  for (let i=0;i<50 && (await object.inspectGrant(pushId)).authorized;i++) await new Promise(resolve=>setTimeout(resolve,10));
  await object.injectStop(stop('push-offline',Date.now()));
  assert((await object.closeReader(push,otherActive.id)).ok,'request completion explicitly closes reader');
  await otherReader.cancel();
  assert(!(await object.hasGrant(otherActive.id)), 'request completion deletes grant');
  const shared = await object.inspectEvent('push-offline');
  const membership = await object.inspectDedupe('push-offline');
  assert(shared.grants.length===1 && shared.grants[0]===pushId && membership.grants.length===1 && membership.grants[0]===pushId,'completion deletes only ended membership');
  const pending=pushReader.read();
  assert(!await Promise.race([pending.then(()=>true),new Promise<boolean>(resolve=>setTimeout(()=>resolve(false),30))]),'offline reader is suspended');
  const reconnect=await object.fetch('http://node/',{headers:{Upgrade:'websocket'}});
  const socket4=reconnect.webSocket; assert(socket4,'push reconnect socket'); socket4.accept();
  const request=await new Promise<string>((resolve,reject)=>{
    const timer=setTimeout(()=>reject(new Error('push authorization timeout')),5000);
    socket4.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize'){clearTimeout(timer);resolve(frame.id);}});
  });
  assert(!await Promise.race([pending.then(()=>true),new Promise<boolean>(resolve=>setTimeout(()=>resolve(false),30))]),'reader waits for reauthorization');
  socket4.send(JSON.stringify({id:request,result:{decision:'denied'}}));
  const terminated=await pending;
  assert(!terminated.done && JSON.parse(new TextDecoder().decode(terminated.value)).reason==='forbidden' && (await pushReader.read()).done && !(await object.hasGrant(pushId)),'denial reports forbidden and deletes grant');
  socket4.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') socket4.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));});
  const expiring=await object.openReader(push,null); assert(expiring.ok,'expiring reader opens');
  const expiryReader=expiring.value.getReader();
  const expiryActive=JSON.parse(new TextDecoder().decode((await expiryReader.read()).value));
  const offlineClosed=new Promise<void>(resolve=>socket4.addEventListener('close',()=>resolve()));
  socket4.close(); await offlineClosed;
  await object.expireAndFire(expiryActive.id);
  const expiryEnd=await expiryReader.read();
  assert(!expiryEnd.done && JSON.parse(new TextDecoder().decode(expiryEnd.value)).reason==='expired' && (await expiryReader.read()).done,'offline expiry closes the reader');
  const resumedSocketResponse=await object.fetch('http://node/',{headers:{Upgrade:'websocket'}});
  const resumedSocket=resumedSocketResponse.webSocket; assert(resumedSocket,'reader tests reconnect'); resumedSocket.accept();
  resumedSocket.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') resumedSocket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));});
  const full=await object.openReader(push,null); assert(full.ok,'full reader opens');
  const fullReader=full.value.getReader();
  const fullActive=JSON.parse(new TextDecoder().decode((await fullReader.read()).value));
  await object.overflowReader(fullActive.id,stop('push-overflow',Date.now()));
  const fullEnd=await fullReader.read();
  assert(!fullEnd.done && JSON.parse(new TextDecoder().decode(fullEnd.value)).reason==='limit' && !(await object.hasGrant(fullActive.id)),'overflow reports limit and removes reader');
  const lost=await object.openReader(push,null); assert(lost.ok,'lost request opens');
  const lostReader=lost.value.getReader();
  const lostActive=JSON.parse(new TextDecoder().decode((await lostReader.read()).value));
  await new Promise(resolve=>setTimeout(resolve,2200));
  await lostReader.cancel();
  for (let i=0;i<40 && await object.hasGrant(lostActive.id);i++) await new Promise(resolve=>setTimeout(resolve,100));
  assert(!(await object.hasGrant(lostActive.id)),'lost stream is reconciled without an event');
  resumedSocket.close();
  const beforeRestart=await object.fetch('http://node/',{headers:{Upgrade:'websocket'}});
  const socket5=beforeRestart.webSocket; assert(socket5,'restart fixture socket'); socket5.accept();
  socket5.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') socket5.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));});
  const hook1={...webhook,callback:'https://example.test/one'};
  const hook2={...webhook,callback:'https://example.test/two'};
  const grant1=await object.subscribe(hook1), grant2=await object.subscribe(hook2);
  assert(grant1.ok && grant2.ok && grant1.value.id!==grant2.value.id,'two webhook grants');
  const grant1Id=grant1.value.id, grant2Id=grant2.value.id;
  await object.setDeliveryStatus(503);
  await object.injectStop(stop('restart-delivery',Date.now()));
  await object.dispatch();
  const beforeDeliveryRestart=await object.deliveryLog();
  assert(beforeDeliveryRestart.length===2,'both webhooks dispatched');
  for (const entry of beforeDeliveryRestart) {
    const message=entry.messages.find((m:{id:string})=>m.id===entry.headers['webhook-id']);
    assert(message && message.attempts===1 && message.due>Date.now(),'attempt reserved durably before request');
  }
  assert(beforeDeliveryRestart[0].headers['webhook-id']!==beforeDeliveryRestart[1].headers['webhook-id'],'standard ids differ');
  const orphan=await object.openReader(push,null); assert(orphan.ok,'reader before restart');
  const orphanReader=orphan.value.getReader();
  const orphanActive=JSON.parse(new TextDecoder().decode((await orphanReader.read()).value));
  const orphanId=JSON.parse(atob(orphanActive.cursor)).grant;
  assert(await object.hasGrant(orphanId),'reader persisted before restart');
  await alarmHarness.update({workers:[{config:{name:'alarm-test',main:'.wrangler/tmp/alarm-fixture.mjs',compatibility_date:'2026-11-01',durable_objects:{bindings:[{name:'NODE',class_name:'TestBroker'}]},migrations:[{tag:'v1',new_sqlite_classes:['TestBroker']}]}}]});
  const restartedEnv=await alarmHarness.getWorker().getEnv();
  const restarted=restartedEnv.NODE.get(restartedEnv.NODE.idFromName('alarm-node'));
  assert(!(await restarted.hasGrant(orphanId)),'restart reconciles lost reader');
  const afterRestart=await restarted.messages();
  assert(afterRestart.length===2 && afterRestart.every((m:{attempts:number})=>m.attempts===1),'attempt reservations survive restart');
  const reconnectDelivery=await restarted.fetch('http://node/',{headers:{Upgrade:'websocket'}});
  const deliverySocket=reconnectDelivery.webSocket; assert(deliverySocket,'delivery reconnect'); deliverySocket.accept();
  deliverySocket.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') deliverySocket.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));});
  for (let i=0;i<100;i++) { const gs=await Promise.all([restarted.inspectGrant(grant1Id),restarted.inspectGrant(grant2Id)]); if(gs.every((g:{authorized:boolean})=>g.authorized)) break; await new Promise(resolve=>setTimeout(resolve,10)); }
  await restarted.setDeliveryStatus(200); await restarted.forceDue(); await restarted.dispatch();
  const afterDeliveryRestart=await restarted.deliveryLog();
  assert(afterDeliveryRestart.length===4,'both retries dispatched after restart');
  for (const original of beforeDeliveryRestart) {
    const retry=afterDeliveryRestart.find((entry:{headers:Record<string,string>;messages:Array<{id:string;attempts:number}>}, i:number)=>i>=2 && entry.headers['webhook-id']===original.headers['webhook-id']);
    assert(retry && retry.messages.some((m:{id:string;attempts:number})=>m.id===original.headers['webhook-id'] && m.attempts===2),'retry retains ID and committed attempt');
  }
  const {createHmac} = await (Function("return import('node:crypto')")() as Promise<{createHmac:(algorithm:string,key:string)=>{update:(text:string)=>{digest:(format:string)=>string}}}>);
  const received=new Set<string>();
  for (const [index,entry] of afterDeliveryRestart.entries()) {
    const id=entry.headers['webhook-id'], timestamp=entry.headers['webhook-timestamp'];
    const signature=createHmac('sha256','a'.repeat(32)).update(`${id}.${timestamp}.${entry.body}`).digest('base64');
    const substituted=createHmac('sha256','a'.repeat(32)).update(`wrong.${timestamp}.${entry.body}`).digest('base64');
    assert(entry.headers['webhook-signature']==='v1,'+signature && signature!==substituted,'independent receiver verifies raw body and actual id');
    assert(received.has(id)===(index>=2),'receiver deduplicates only retries'); received.add(id);
    assert(JSON.parse(entry.body).eventId==='restart-delivery','both subscriptions carry same occurrence');
  }
  assert((await restarted.unsubscribe(hook2)).ok,'isolate loss fixture');
  const logBeforeLoss=await restarted.deliveryLog();
  await restarted.setFault('before'); await restarted.injectStop(stop('loss-before',Date.now()));
  try { await restarted.dispatch(); } catch { /* simulated DO loss */ }
  const reservedBefore=(await restarted.messages()).find((m:{eventId:string})=>m.eventId==='loss-before');
  assert(reservedBefore?.attempts===1,'loss before request leaves committed reservation');
  assert((await restarted.deliveryLog()).length===logBeforeLoss.length,'loss before request makes no HTTP call');
  await restarted.setFault('after'); await restarted.injectStop(stop('loss-after',Date.now()));
  try { await restarted.dispatch(); } catch { /* receiver accepted, result was lost */ }
  const reservedAfter=(await restarted.messages()).find((m:{eventId:string})=>m.eventId==='loss-after');
  assert(reservedAfter?.attempts===1,'loss after acceptance leaves committed reservation');
  const lossLog=await restarted.deliveryLog();
  assert(lossLog.some((x:{body:string})=>JSON.parse(x.body).eventId==='loss-after'),'receiver accepted before result loss');
  const beforeId=reservedBefore.id, afterId=reservedAfter.id;
  deliverySocket.close();
  await alarmHarness.update({workers:[{config:{name:'alarm-test',main:'.wrangler/tmp/alarm-fixture.mjs',compatibility_date:'2026-11-01',durable_objects:{bindings:[{name:'NODE',class_name:'TestBroker'}]},migrations:[{tag:'v1',new_sqlite_classes:['TestBroker']}]}}]});
  const recoveredEnv=await alarmHarness.getWorker().getEnv();
  const recovered=recoveredEnv.NODE.get(recoveredEnv.NODE.idFromName('alarm-node'));
  const persisted=await recovered.messages();
  assert(persisted.some((m:{id:string;attempts:number})=>m.id===beforeId && m.attempts===1) && persisted.some((m:{id:string;attempts:number})=>m.id===afterId && m.attempts===1),'both ambiguous reservations survive restart');
  const rc=await recovered.fetch('http://node/',{headers:{Upgrade:'websocket'}}); const rs=rc.webSocket; assert(rs,'loss recovery socket'); rs.accept();
  rs.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') rs.send(JSON.stringify({id:frame.id,result:{decision:'allowed'}}));});
  for (let i=0;i<100;i++) { if ((await recovered.inspectGrant(grant1Id)).authorized) break; await new Promise(resolve=>setTimeout(resolve,10)); }
  await recovered.forceDue(); await recovered.dispatch();
  const recoveredLog=await recovered.deliveryLog();
  for (const id of [beforeId,afterId]) {
    const retry=recoveredLog.find((x:{headers:Record<string,string>;messages:Array<{id:string;attempts:number}>})=>x.headers['webhook-id']===id && x.messages.some((m:{id:string;attempts:number})=>m.id===id && m.attempts===2));
    assert(retry,'retry retains id and counts ambiguous attempt');
  }
  const beforeTermination=await recovered.deliveryLog();
  rs.close();
  const denialConnection=await recovered.fetch('http://node/',{headers:{Upgrade:'websocket'}});
  const denialSocket=denialConnection.webSocket; assert(denialSocket,'webhook denial socket'); denialSocket.accept();
  denialSocket.addEventListener('message',(message:{data:string})=>{const frame=JSON.parse(message.data); if(frame.method==='events.authorize') denialSocket.send(JSON.stringify({id:frame.id,result:{decision:'denied'}}));});
  for (let i=0;i<100 && await recovered.hasGrant(grant1Id);i++) await new Promise(resolve=>setTimeout(resolve,10));
  assert(!(await recovered.hasGrant(grant1Id)),'webhook denial removes grant');
  await recovered.dispatch();
  const terminationLog=(await recovered.deliveryLog()).slice(beforeTermination.length);
  assert(terminationLog.length===1 && JSON.parse(terminationLog[0].body).type==='terminated','denial sends one termination control');
  const entry=terminationLog[0], id=entry.headers['webhook-id'], timestamp=entry.headers['webhook-timestamp'];
  assert(id.startsWith('msg_terminated_') && entry.headers['webhook-signature']==='v1,'+createHmac('sha256','a'.repeat(32)).update(`${id}.${timestamp}.${entry.body}`).digest('base64'),'independent receiver verifies termination');
  await recovered.dispatch();
  assert((await recovered.deliveryLog()).length===beforeTermination.length+1,'accepted termination is not repeated');
  denialSocket.close();
} finally {
  await alarmHarness.close();
}
console.log('real DO alarm test passed');
