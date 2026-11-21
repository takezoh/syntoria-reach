import { safeHttps, secretBytes, sign, type Resolver, type SafeHttps, type Transport } from './webhook.ts';
const { strict: assert } = await (Function("return import('node:assert')")() as Promise<any>);
const { createHmac } = await (Function("return import('node:crypto')")() as Promise<any>);
const { Buffer } = await (Function("return import('node:buffer')")() as Promise<any>);

const secret = 'whsec_' + btoa('a'.repeat(32));
const url = 'https://example.test/callback';
const good = ['8.8.8.8','2606:4700:4700::1111'];
let calls: Array<{url:string; init:RequestInit}> = [];
let addresses = good;
let response = 200;
let echo = true;
const answer = () => ({a:addresses.filter(x=>!x.includes(':')),aaaa:addresses.filter(x=>x.includes(':'))});
let resolver:Resolver = async () => answer();
const transport:Transport = async (target,init) => {
  calls.push({url:target,init});
  const headers = new Headers(init.headers), body = String(init.body);
  const expected = createHmac('sha256',Buffer.from(secret.slice(6),'base64')).update(`${headers.get('webhook-id')}.${headers.get('webhook-timestamp')}.${body}`).digest('base64');
  assert.equal(headers.get('webhook-signature'),'v1,'+expected);
  assert.equal(headers.get('x-mcp-subscription-id'),'sub');
  assert.equal(headers.get('authorization'),null);
  assert.equal(init.redirect,'manual');
  return new Response(JSON.stringify({challenge:echo ? JSON.parse(body).challenge : 'wrong'}),{status:response,headers:{Location:'https://other.example.test/'}});
};
const id = 'msg_verification_123', nonce = 'nonce';
async function verify(port:SafeHttps, target=url) { return port.verify(target,secret,'sub',nonce,id,100000); }
const port = () => safeHttps('https://example.test',resolver,transport);
for (const issuer of ['https://access.example.test', 'http://example.test', 'http://127.0.0.1:4444/path']) {
  let invoked = false;
  const p = safeHttps('http://127.0.0.1:4321', async () => { invoked=true; return answer(); }, async () => { invoked=true; return new Response(); }, issuer);
  assert(!(await verify(p,'http://127.0.0.1:4321/callback')).ok);
  assert.equal(invoked,false);
}
{
  let resolved = false;
  const p = safeHttps('http://127.0.0.1:4321', async () => { resolved=true; return answer(); }, transport, 'http://127.0.0.1:4444');
  assert((await verify(p,'http://127.0.0.1:4321/callback')).ok);
  assert.equal(resolved,false);
  const denied = await verify(p,'http://127.0.0.1:4322/callback');
  assert(!denied.ok && denied.error === 'callback_endpoint');
  assert.equal(resolved,false);
  calls=[];
}
assert(secretBytes(secret));
assert.equal(secretBytes('whsec_YQ=='),null);
const signature = await sign(secret,id,'100','body');
function independent(i:string,t:string,b:string) { return 'v1,'+createHmac('sha256',Buffer.from(secret.slice(6),'base64')).update(`${i}.${t}.${b}`).digest('base64'); }
assert.equal(signature,independent(id,'100','body'));
assert.notEqual(signature,independent(id+'x','100','body'));
assert.notEqual(signature,independent(id,'101','body'));
assert.notEqual(signature,independent(id,'100','other'));
assert((await verify(port())).ok); assert.equal(calls.length,1);
for (const [target,allow] of [[url,undefined],[url,''],['http://example.test','https://example.test'],['https://127.0.0.1','https://127.0.0.1'],['https://[::1]','https://[::1]'],['https://example.test.evil.test','https://example.test'],['https://example.test:8443','https://example.test'],['https://other.example.test','https://example.test'],['https://x@example.test','https://example.test']] as const) {
  calls=[]; let resolved=0;
  const result=await verify(safeHttps(allow,async()=>{resolved++;return {a:[good[0]],aaaa:[good[1]]};},transport),target);
  assert(!result.ok); assert.equal(resolved,0); assert.equal(calls.length,0);
}
for (const answer of [['10.1.2.3'],['fc00::1'],['::ffff:10.1.2.3'],['8.8.8.8','127.0.0.1'],['192.168.1.1'],[]]) {
  calls=[]; addresses=answer; const result=await verify(port()); assert(!result.ok); assert.equal(calls.length,0);
}
addresses=good; calls=[]; resolver=async()=>{throw new Error('failed')}; assert(!(await verify(port())).ok); assert.equal(calls.length,0);
resolver=async()=>({a:['8.8.8.8']} as unknown as {a:string[];aaaa:string[]}); const incomplete=await verify(port()); assert(!incomplete.ok && incomplete.error==='resolution_failed'); assert.equal(calls.length,0);
resolver=async()=>{throw new DOMException('timeout','TimeoutError')}; const timed=await verify(port()); assert(!timed.ok && timed.error==='timeout'); assert.equal(calls.length,0);
resolver=async()=>({a:[good[0]],aaaa:[good[1]]}); calls=[]; response=302; assert(!(await verify(port())).ok); assert.equal(calls.length,1);
response=200; echo=false; calls=[]; const wrong=await verify(port()); assert(!wrong.ok && wrong.error==='challenge_failed');
echo=true; calls=[];
const tls = await verify(safeHttps('https://example.test',resolver,async()=>{throw new Error('TLS certificate error');}));
assert(!tls.ok && tls.error==='tls_error');
let count=0;
const changing = safeHttps('https://example.test',async()=>({a:[++count === 1 ? '8.8.8.8' : '10.0.0.1'],aaaa:[]}),transport);
assert((await verify(changing)).ok);
calls=[]; assert(!(await verify(changing)).ok); assert.equal(calls.length,0);
console.log('webhook tests passed');

// Exercise the production resolver in workerd against recorded DoH responses,
// and compare the same outcomes with a resolver fake.
const { build } = await (Function("return import('esbuild')")() as Promise<any>);
const { createTestHarness } = await (Function("return import('wrangler')")() as Promise<any>);
const fs = await (Function("return import('node:fs/promises')")() as Promise<any>);
const dnsBundle = await build({stdin:{contents:`
import {safeHttps,dohResolver} from './src/webhook.ts';
export default {async fetch(request) {
  const test = await request.json();
  const queries = [];
  const recorded = async (url,init) => {
    // Build the Request as the real fetch would, so options workerd rejects fail here too.
    new Request(url, init);
    if (init.redirect !== 'manual') throw Error('DNS request must not follow redirects');
    const u = new URL(url);
    if (u.origin !== 'https://cloudflare-dns.com' || u.pathname !== '/dns-query' || u.searchParams.get('name') !== 'example.test' || new Headers(init.headers).get('accept') !== 'application/dns-json') throw Error('unexpected DNS request');
    const type = u.searchParams.get('type'); queries.push(type);
    const reply = test.responses[type];
    if (reply.timeout) throw new DOMException('timed out','TimeoutError');
    return new Response(reply.raw ?? JSON.stringify(reply.json),{status:reply.status ?? 200});
  };
  let calls = 0;
  const callback = async (_url,init) => { calls++; return new Response(JSON.stringify({challenge:JSON.parse(init.body).challenge})); };
  const resolver = test.fake ? async () => { if (test.fake.timeout) throw new DOMException('timed out','TimeoutError'); if (test.fake.fail) throw Error('DNS failed'); return test.fake.answer; } : dohResolver(recorded);
  const outcome = await safeHttps('https://example.test',resolver,callback).verify('https://example.test/callback','whsec_'+btoa('a'.repeat(32)),'sub','nonce','msg_verification_123',100000);
  return Response.json({outcome,calls,queries});
}};
`,resolveDir:Function('return process.cwd()')(),sourcefile:'dns-fixture.ts'},bundle:true,write:false,format:'esm',platform:'browser'});
await fs.mkdir('.wrangler/tmp',{recursive:true});
await fs.writeFile('.wrangler/tmp/dns-fixture.mjs',dnsBundle.outputFiles[0].text);
const dnsHarness = createTestHarness({workers:[{config:{name:'dns-test',main:'.wrangler/tmp/dns-fixture.mjs',compatibility_date:'2026-11-01'}}]});
const cname = {name:'example.test',type:5,data:'alias.example.test.'};
const a = {name:'alias.example.test',type:1,data:'8.8.8.8'};
const aaaa = {name:'alias.example.test',type:28,data:'2606:4700:4700::1111'};
const privateA = {name:'alias.example.test',type:1,data:'10.0.0.1'};
const dns = (answer:unknown[] = [], status=0) => ({json:{Status:status,Answer:answer}});
const scenarios = [
  {name:'A-only', A:dns([cname,a]), AAAA:dns([cname]), fake:{answer:{a:[a.data],aaaa:[]}}, expected:'ok'},
  {name:'AAAA-only', A:dns(), AAAA:dns([{...aaaa,name:'example.test'}]), fake:{answer:{a:[],aaaa:[aaaa.data]}}, expected:'ok'},
  {name:'alias public', A:dns([cname,a]), AAAA:dns([cname,aaaa]), fake:{answer:{a:[a.data],aaaa:[aaaa.data]}}, expected:'ok'},
  {name:'alias private', A:dns([cname,privateA]), AAAA:dns(), fake:{answer:{a:[privateA.data],aaaa:[]}}, expected:'callback_endpoint'},
  {name:'empty', A:dns(), AAAA:dns(), fake:{answer:{a:[],aaaa:[]}}, expected:'resolution_failed'},
  {name:'NXDOMAIN', A:dns([],3), AAAA:dns([],3), fake:{fail:true}, expected:'resolution_failed'},
  {name:'SERVFAIL', A:dns([],2), AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
  {name:'bad JSON', A:{raw:'{'}, AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
  {name:'HTTP error', A:{status:503,json:{}}, AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
  {name:'timeout', A:{timeout:true}, AAAA:dns(), fake:{timeout:true}, expected:'timeout'},
  {name:'truncated', A:{json:{Status:0,TC:true}}, AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
  {name:'unrelated address', A:dns([a]), AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
  {name:'alias loop', A:dns([cname,{name:'alias.example.test',type:5,data:'example.test.'}]), AAAA:dns(), fake:{fail:true}, expected:'resolution_failed'},
];
try {
  await dnsHarness.listen();
  for (const scenario of scenarios) {
    for (const fake of [false,true]) {
      const res = await dnsHarness.getWorker().fetch('http://fixture/',{method:'POST',body:JSON.stringify({responses:{A:scenario.A,AAAA:scenario.AAAA},fake:fake ? scenario.fake : null})});
      assert.equal(res.status,200,`${scenario.name}: fixture response`);
      const result = await res.json() as {outcome:{ok:boolean;error?:string};calls:number;queries:string[]};
      assert.equal(result.outcome.ok ? 'ok' : result.outcome.error,scenario.expected,`${scenario.name}: ${fake ? 'fake' : 'workerd adapter'}`);
      assert.equal(result.calls,scenario.expected === 'ok' ? 1 : 0,`${scenario.name}: callback count`);
      if (!fake) assert.deepEqual(result.queries.sort(),['A','AAAA'],`${scenario.name}: both families`);
    }
  }
} finally { await dnsHarness.close(); }
console.log('workerd DNS contract tests passed');

// The receiver computes HMAC independently from the exact request bytes and
// rejects an ID substituted after signing. It deduplicates across subscriptions.
{
  const {createHmac} = await (Function("return import('node:crypto')")() as Promise<{createHmac:(algorithm:string,key:string)=>{update:(text:string)=>{digest:(format:string)=>string}}}>);
  const secret = 'whsec_' + btoa('a'.repeat(32));
  const seen = new Set<string>();
  const deliveries:Array<{id:string;body:string;subscription:string;duplicate:boolean}> = [];
  let address = '8.8.8.8'; let status = 200; let requests = 0;
  const port = safeHttps('https://example.test', async()=>({a:[address],aaaa:[]}), async (_url,init)=>{
    requests++;
    const headers = new Headers(init.headers); const id=headers.get('webhook-id')!;
    const timestamp=headers.get('webhook-timestamp')!; const body=String(init.body);
    const mac = createHmac('sha256', 'a'.repeat(32)).update(`${id}.${timestamp}.${body}`).digest('base64');
    assert(headers.get('webhook-signature')===`v1,${mac}`,'receiver verifies actual id and body');
    assert(headers.get('webhook-signature')!==`v1,${createHmac('sha256','a'.repeat(32)).update(`substituted.${timestamp}.${body}`).digest('base64')}`,'substituted id fails');
    assert(init.redirect==='manual' && !headers.has('authorization') && !headers.has('cf-access-jwt-assertion'),'safe callback headers');
    deliveries.push({id,body,subscription:headers.get('X-MCP-Subscription-Id')!,duplicate:seen.has(id)}); seen.add(id);
    return new Response(null,{status});
  });
  const body=JSON.stringify({eventId:'same-event',name:'agent.session_state_changed',timestamp:'2026-01-01T00:00:00Z',data:{node:'node',session_id:'session',state:'idle',observed_at:'2026-01-01T00:00:00Z'},cursor:'cursor'});
  await port.deliver('https://example.test/hook',secret,'sub1','random1',1000,body,Date.now()+10000,async()=>true);
  await port.deliver('https://example.test/hook',secret,'sub2','random2',1000,body,Date.now()+10000,async()=>true);
  await port.deliver('https://example.test/hook',secret,'sub1','random1',2000,body,Date.now()+10000,async()=>true);
  assert(deliveries.length===3 && !deliveries[0].duplicate && !deliveries[1].duplicate && deliveries[2].duplicate,'receiver deduplicates retries but not subscriptions');
  for (const terminal of [410,413]) { status=terminal; const result=await port.deliver('https://example.test/hook',secret,'sub1','id',3000,body,Date.now()+10000,async()=>true); assert(result.status==='terminal' && result.category==='http_4xx','terminal HTTP status'); }
  status=503; let result=await port.deliver('https://example.test/hook',secret,'sub1','id',3000,body,Date.now()+10000,async()=>true); assert(result.status==='retryable' && result.category==='http_5xx','503 retryable');
  const revokedBefore=requests; result=await port.deliver('https://example.test/hook',secret,'sub1','id',3000,body,Date.now()+10000,async()=>false);
  assert(result.status==='deferred' && requests===revokedBefore,'revoked grant prevents request without inventing callback error');
  const before=requests; address='127.0.0.1'; result=await port.deliver('https://example.test/hook',secret,'sub1','id',4000,body,Date.now()+10000,async()=>true);
  assert(result.status==='terminal' && requests===before,'fresh private DNS answer prevents request');
}
