function assert(value: unknown): asserts value { if (!value) throw new Error('assertion failed'); }
assert.equal = (a:unknown,b:unknown) => { if (a !== b) throw new Error(`expected ${String(b)}, got ${String(a)}`); };
const canonical = (x:any):any => Array.isArray(x) ? x.map(canonical) : x && typeof x === 'object' ? Object.fromEntries(Object.keys(x).sort().map(k => [k,canonical(x[k])])) : x;
assert.deepEqual = (a:unknown,b:unknown) => { if (JSON.stringify(canonical(a)) !== JSON.stringify(canonical(b))) throw new Error(`expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}`); };
import { eventsWire } from './events-wire.ts';
import * as events from './events.ts';

const principal = {issuer:'https://issuer.invalid',subject:'owner'};
const nodes = new Set(['node']);
const calls: Array<{input:events.GrantInput;options?:events.WebhookOptions}> = [];
const broker = () => ({
  async subscribe(input:events.GrantInput, options?:events.WebhookOptions) { calls.push({input,options}); return {ok:true as const,value:{id:'sub',refreshBefore:'2026-01-01T00:00:00Z',cursor:'c',truncated:false}}; },
  async unsubscribe(input:events.GrantInput) { calls.push({input}); return {ok:true as const,value:null}; },
  async poll(input:events.GrantInput, cursor:string|null) { calls.push({input}); return {ok:true as const,value:{events:[],cursor:cursor ?? 'now',truncated:cursor === 'stale',hasMore:false,nextPollMs:1000}}; },
  async openReader(input:events.GrantInput) { calls.push({input}); return {ok:true as const,value:new ReadableStream<Uint8Array>()}; },
});
const params = {name:'agent.session_state_changed',arguments:{session_id:'s',node:'node'},delivery:{mode:'webhook',url:'https://callback.invalid',secret:'secret'},ttlMs:60000,cursor:null};
for (const profile of ['standard','chatgpt'] as const) {
  const list = await eventsWire('events/list',{},profile,principal,nodes,broker);
  assert.deepEqual((list.result as any).events[0].delivery,profile === 'standard' ? ['poll','push','webhook'] : ['webhook']);
  const sub = await eventsWire('events/subscribe',params,profile,principal,nodes,broker);
  assert.equal((sub.result as any).id,'sub');
  assert.equal(calls.at(-1)?.input.profile,profile);
  assert.deepEqual(calls.at(-1)?.input.principal,principal);
  assert.equal(calls.at(-1)?.options?.ttlMs,60000);
  assert.deepEqual((await eventsWire('events/unsubscribe',params,profile,principal,nodes,broker)).result,{});
}
assert.equal((await eventsWire('events/poll',{...params,cursor:null},'standard',principal,nodes,broker)).error,undefined);
assert.equal(((await eventsWire('events/poll',{...params,cursor:'stale'},'standard',principal,nodes,broker)).result as any).truncated,true);
assert.equal((await eventsWire('events/poll',params,'chatgpt',principal,nodes,broker)).error?.code,-32601);
assert.equal((await eventsWire('events/subscribe',{...params,arguments:{node:'missing',session_id:'s'}},'standard',principal,nodes,broker)).error?.code,-32000);
const input:events.GrantInput = {...calls[0].input, secret:'secret'};
let [state, first] = events.subscribe(events.empty(),input,1000,'id','allowed',{ttlMs:1,cursor:'old'},61000);
assert(first.ok); assert.equal(first.value.expires,61000); assert.equal(first.value.truncated,true);
assert.equal(events.lease({ttlMs:null}),300000);
assert.equal(events.lease({ttlMs:500000}),300000);
assert.equal(events.lease({maxAgeMs:-1}),null);
const current=events.watermark(state,first.value);
[state,first]=events.subscribe(state,input,2000,'id','allowed',{cursor:current},62000);
assert(first.ok); assert.equal(first.value.id,'id');
[state,first]=events.subscribe(state,input,3000,'id','allowed',{cursor:'foreign'},63000);
assert(first.ok); assert.equal(first.value.truncated,true);
console.log('events wire tests passed');

// Exercise the HTTP adapter with a broker fake and the real SDK tool handlers.
const build = (await (Function("return import('esbuild')")() as Promise<any>)).build;
const fs = await (Function("return import('node:fs/promises')")() as Promise<any>);
const os = await (Function("return import('node:os')")() as Promise<any>);
const url = await (Function("return import('node:url')")() as Promise<any>);
const temporary = await fs.mkdtemp(os.tmpdir() + '/reach-wire-');
try {
  await build({entryPoints:['src/mcp.ts'],bundle:true,platform:'node',format:'esm',outfile:temporary+'/wire.mjs',plugins:[{name:'broker-fake',setup(b:any){
    b.onResolve({filter:/^\.\/broker$/},()=>({path:'broker',namespace:'fake'}));
    b.onLoad({filter:/.*/,namespace:'fake'},()=>({contents:`export function nodeBroker(env, node) { return env.broker; }
export function failure(code,message) { return {error:{code,message}}; }
export function unknownOutcome() { return failure('result_unknown','unknown'); }`,loader:'js'}));
  }}]});
  const {mcp} = await import(url.pathToFileURL(temporary+'/wire.mjs').href);
  await build({entryPoints:['src/index.ts'],bundle:true,platform:'node',format:'esm',outfile:temporary+'/index.mjs',plugins:[{name:'broker-fake',setup(b:any){
    b.onResolve({filter:/^\.\/broker$/},()=>({path:'broker',namespace:'fake'}));
    b.onLoad({filter:/.*/,namespace:'fake'},()=>({contents:`export class NodeBroker {};
export function nodeBroker(env) { return env.broker; }
export function failure(code,message) { return {error:{code,message}}; }
export function unknownOutcome() { return failure('result_unknown','unknown'); }`,loader:'js'}));
  }}]});
  const {default: gateway} = await import(url.pathToFileURL(temporary+'/index.mjs').href);
  const jose = await (Function("return import('jose')")() as Promise<any>);
  const keys = await jose.generateKeyPair('RS256');
  const jwk = {...await jose.exportJWK(keys.publicKey),kid:'test',alg:'RS256'};
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => Response.json({keys:[jwk]});
  try {
    const settings = {ACCESS_TEAM_DOMAIN:'https://issuer.invalid',ACCESS_AUD:'aud',ACCESS_OWNER_EMAIL:'owner@example.invalid',NODES:JSON.stringify({node:'x'.repeat(40)}),broker:broker()};
    async function token(email:string, issuer='https://issuer.invalid') {
      return new jose.SignJWT({email}).setProtectedHeader({alg:'RS256',kid:'test'}).setIssuer(issuer).setAudience('aud').setSubject('owner-subject').setIssuedAt().setExpirationTime('5m').sign(keys.privateKey);
    }
    async function access(jwt?:string, issuer='https://issuer.invalid') {
      const headers:Record<string,string> = {'content-type':'application/json'};
      if (jwt) headers['Cf-Access-Jwt-Assertion'] = jwt;
      return gateway.fetch(new Request('https://gateway.invalid/mcp?profile=chatgpt',{method:'POST',headers,body:JSON.stringify({jsonrpc:'2.0',id:1,method:'events/subscribe',params})}),{...settings,ACCESS_TEAM_DOMAIN:issuer});
    }
    const before = calls.length;
    assert.equal((await access()).status,401);
    assert.equal((await access(await token('other@example.invalid'))).status,403);
    globalThis.fetch = async () => { throw new Error('unavailable'); };
    assert.equal((await access(await token('owner@example.invalid','https://offline.invalid'),'https://offline.invalid')).status,503);
    assert.equal(calls.length,before);
    globalThis.fetch = async () => Response.json({keys:[jwk]});
    assert.equal((await access(await token('owner@example.invalid'))).status,200);
    assert.deepEqual(calls.at(-1)?.input.principal,{issuer:'https://issuer.invalid',subject:'owner-subject'});
    assert.equal(calls.at(-1)?.input.profile,'chatgpt');
  } finally { globalThis.fetch = originalFetch; }

  const env = {broker:broker()};
  async function http(method:string, params:unknown, profile: 'standard'|'chatgpt', revision:string) {
    const req = new Request('https://gateway.invalid/mcp'+(profile === 'chatgpt' ? '?profile=chatgpt' : ''), {method:'POST',headers:{'content-type':'application/json','accept':'application/json, text/event-stream','mcp-protocol-version':revision,...(profile === 'standard' && (params as any)?.name ? {'mcp-name':(params as any).name} : {})},body:JSON.stringify({jsonrpc:'2.0',id:1,method,params})});
    const response = await mcp(req,env,nodes,principal);
    assert.equal(response.status,200);
    return response.json() as Promise<any>;
  }
  const old = '2025-11-25', current = '2026-07-28';
  for (const profile of ['standard','chatgpt'] as const) {
    const discover = await http('server/discover',{},profile,current);
    assert(profile === 'chatgpt' ? discover.result.capabilities.events : discover.result.capabilities.extensions['io.modelcontextprotocol/events']);
    const listed = await http('events/list',{},profile,current);
    assert.deepEqual(listed.result.events[0].delivery,profile === 'chatgpt' ? ['webhook'] : ['poll','push','webhook']);
    const oldTools = (await http('tools/list',{},profile,old)).result.tools;
    const newTools = (await http('tools/list',{},profile,current)).result.tools;
    assert.deepEqual(newTools,oldTools);
    for (const args of [{name:'list_nodes',arguments:{}},{name:'nope',arguments:{}},{name:'read_agent_output',arguments:{}}]) {
      const a=(await http('tools/call',args,profile,old)).result;
      const b=(await http('tools/call',args,profile,current)).result;
      delete b.resultType;
      assert.deepEqual(b,a);
    }
    const sub = await http('events/subscribe',params,profile,current);
    assert.equal(sub.result.id,'sub'); assert.equal(calls.at(-1)?.input.profile,profile);
  }
  for (const profile of ['standard','chatgpt'] as const) {
    for (const [category,code] of [['invalid_params',-32602],['not_found',-32023],['forbidden',-32024],['limit',-32025],['challenge_failed',profile === 'standard' ? -32027 : -32015],['unavailable',-32000]] as const) {
      env.broker.subscribe = async () => ({ok:false,error:category}) as any;
      const response = await http('events/subscribe',params,profile,current);
      assert.equal(response.error.code,code);
      if (category === 'limit') assert.equal(response.error.data.limit,'subscriptions');
      if (category === 'challenge_failed') assert.equal(response.error.data.reason,'challenge_failed');
      assert.equal(response.result,undefined);
    }
    assert.equal((await http('events/subscribe',{...params,delivery:{...params.delivery,mode:'push'}},profile,current)).error.code,-32026);
  }
  env.broker = broker();
  assert.equal((await http('events/list',{},'standard',old)).error.code,-32601);
  const init = await http('initialize',{protocolVersion:old,capabilities:{},clientInfo:{name:'test',version:'1'}},'chatgpt',old);
  assert.deepEqual(init.result.capabilities.events,{});
  assert.equal((await http('events/poll',{...params,cursor:null},'chatgpt',current)).error.code,-32601);

  // An occurrence reaches the caller with only the payload fields, through HTTP.
  env.broker.poll = async () => ({ok:true,value:{events:[{type:'turn_stop',eventId:'evt_1',session_id:'s',state:'idle',observed_at:'2026-01-01T00:00:00.000Z'}],cursor:'next',truncated:false,hasMore:false,nextPollMs:1000}}) as any;
  const polled = await http('events/poll',{...params,cursor:'c'},'standard',current);
  assert.deepEqual(polled.result.events,[{eventId:'evt_1',name:'agent.session_state_changed',timestamp:'2026-01-01T00:00:00.000Z',data:{node:'node',session_id:'s',state:'idle',observed_at:'2026-01-01T00:00:00.000Z'}}]);
  assert.equal(polled.result.cursor,'next');
  env.broker = broker();

  // ChatGPT unsubscribe is idempotent; the standard profile reports NotFound.
  env.broker.unsubscribe = async () => ({ok:false,error:'not_found'}) as any;
  assert.deepEqual((await http('events/unsubscribe',params,'chatgpt',current)).result,{resultType:'complete'});
  assert.equal((await http('events/unsubscribe',params,'standard',current)).error.code,-32023);
  env.broker = broker();

  // Inspect the actual SSE response and cancellation at the HTTP boundary.
  let cancelled = false;
  const enc = new TextEncoder();
  (env.broker as any).openReader = async () => ({ok:true,value:new ReadableStream({start(c) {
    for (const record of [{type:'active',cursor:'first',truncated:false},{type:'event',cursor:'next',event:{eventId:'e',session_id:'s',state:'idle',observed_at:'2026-01-01T00:00:00Z',reply:'secret'},extra:'secret'},{type:'terminated',reason:'limit'}]) c.enqueue(enc.encode(JSON.stringify(record)+'\n'));
    c.close();
  },cancel() { cancelled = true; }})});
  const streamRequest = (headers:Record<string,string>, signal?:AbortSignal, cursor:string|null = null) => new Request('https://gateway.invalid/mcp',{method:'POST',headers:{'content-type':'application/json','mcp-protocol-version':current,...headers},signal,body:JSON.stringify({jsonrpc:'2.0',id:9,method:'events/stream',params:{...params,cursor,heartbeatMs:10}})});
  for (const method of ['events/poll','events/stream','events/subscribe','events/unsubscribe']) {
    for (const name of [undefined,'wrong']) {
      const headers:Record<string,string> = {'content-type':'application/json','mcp-protocol-version':current};
      if (name) headers['mcp-name'] = name;
      const before = calls.length;
      const r = await mcp(new Request('https://gateway.invalid/mcp',{method:'POST',headers,body:JSON.stringify({jsonrpc:'2.0',id:1,method,params})}),env,nodes,principal);
      assert.equal(r.status,400); assert.equal((await r.json() as any).error.code,-32020); assert.equal(calls.length,before);
    }
  }
  const missing = await mcp(streamRequest({}),env,nodes,principal);
  assert.equal(missing.status,400); assert.equal((await missing.json() as any).error.code,-32020);
  const meta = {'io.modelcontextprotocol/subscriptionId':9};
  const result = {jsonrpc:'2.0',id:9,result:{resultType:'complete'}};
  const notification = (method:string, data:object) => ({jsonrpc:'2.0',method:'notifications/events/'+method,params:{...data,_meta:meta}});
  const response = await mcp(streamRequest({'mcp-name':params.name}),env,nodes,principal);
  const frames = (await response.text()).trim().split('\n\n').map((x:string) => JSON.parse(x.slice(6)));
  assert.deepEqual(frames,[
    notification('active',{cursor:'first',truncated:false,heartbeatMs:1000}),
    notification('event',{eventId:'e',name:params.name,timestamp:'2026-01-01T00:00:00Z',data:{node:'node',session_id:'s',state:'idle',observed_at:'2026-01-01T00:00:00Z'},cursor:'next'}),
    notification('terminated',{error:{code:-32025,message:'Resource exhausted',data:{limit:'events',max:events.MAX_EVENTS}}}),result,
  ]);
  (env.broker as any).openReader = async () => ({ok:true,value:new ReadableStream({start(c) {
    c.enqueue(enc.encode(JSON.stringify({type:'terminated',reason:'forbidden'})+'\n')); c.close();
  }})});
  const denied = await mcp(streamRequest({'mcp-name':params.name}),env,nodes,principal);
  assert.deepEqual((await denied.text()).trim().split('\n\n').map((x:string) => JSON.parse(x.slice(6))),[notification('terminated',{error:{code:-32024,message:'Forbidden'}}),result]);

  let receivedCursor: string|null = null;
  (env.broker as any).openReader = async (_input:events.GrantInput, cursor:string|null) => {
    receivedCursor = cursor;
    return {ok:true,value:new ReadableStream({start(c) { c.enqueue(enc.encode(JSON.stringify({type:'active',cursor:'fresh',truncated:cursor !== null})+'\n')); },cancel() { cancelled = true; }})};
  };
  const aborter = new AbortController();
  const live = await mcp(streamRequest({'mcp-name':params.name},aborter.signal,'old-cursor'),env,nodes,principal);
  const liveReader = live.body!.getReader();
  const frame = async () => JSON.parse(new TextDecoder().decode((await liveReader.read()).value).slice(6));
  assert.deepEqual(await frame(),notification('active',{cursor:'fresh',truncated:true,heartbeatMs:1000}));
  assert.equal(receivedCursor,'old-cursor');
  assert.deepEqual(await frame(),notification('heartbeat',{cursor:'fresh'}));
  aborter.abort(); await new Promise(resolve => setTimeout(resolve,0)); assert(cancelled);

  // Keep an event in the broker until the response is consumed or cancelled.
  for (const mode of ['signal','body'] as const) {
    let reads = 0, cancels = 0, agentCalls = 0;
    (env.broker as any).openReader = async () => ({ok:true,value:new ReadableStream({pull(c) {
      reads++;
      c.enqueue(enc.encode(JSON.stringify(reads === 1 ? {type:'active',cursor:'first',truncated:false} : {type:'event',cursor:'next',event:{eventId:'e',session_id:'s',state:'idle',observed_at:'2026-01-01T00:00:00Z'}})+'\n'));
    },cancel() { cancels++; }},{highWaterMark:0})});
    (env.broker as any).call = () => { agentCalls++; };
    const controller = new AbortController();
    const r = await mcp(streamRequest({'mcp-name':params.name},controller.signal),env,nodes,principal);
    const body = r.body!.getReader();
    assert.deepEqual(JSON.parse(new TextDecoder().decode((await body.read()).value).slice(6)),notification('active',{cursor:'first',truncated:false,heartbeatMs:1000}));
    if (mode === 'signal') { controller.abort(); const ended = await body.read().then((r: ReadableStreamReadResult<Uint8Array>) => r.done, () => true); assert(ended); }
    else await body.cancel();
    await new Promise(resolve => setTimeout(resolve,0));
    assert.equal(reads,1); assert.equal(cancels,1); assert.equal(agentCalls,0);
  }

  // A notification gets no response; a tools/call without a name is a protocol error.
  const notice = await mcp(new Request('https://gateway.invalid/mcp',{method:'POST',headers:{'content-type':'application/json','accept':'application/json, text/event-stream','mcp-protocol-version':current},body:JSON.stringify({jsonrpc:'2.0',method:'notifications/cancelled',params:{requestId:1}})}),env,nodes,principal);
  assert.equal(notice.status,202);
  assert.equal(await notice.text(),'');
  for (const profile of ['standard','chatgpt'] as const) assert.equal((await http('tools/call',{arguments:{}},profile,current)).error.code,-32602);
} finally { await fs.rm(temporary,{recursive:true,force:true}); }
console.log('raw HTTP wire tests passed');
