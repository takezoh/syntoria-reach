import ipaddr from 'ipaddr.js';

export type WebhookError = 'invalid_params' | 'callback_endpoint' | 'resolution_failed' | 'timeout' | 'tls_error' | 'connection_refused' | 'challenge_failed';
export type Outcome<T> = {ok:true; value:T} | {ok:false; error:WebhookError};
export type Resolver = (hostname:string, signal?:AbortSignal) => Promise<{a:string[]; aaaa:string[]}>;
export type Transport = (url:string, init:RequestInit) => Promise<Response>;
export type SafeHttps = { verify(url:string, secret:string, subscriptionId:string, nonce:string, messageId:string, now:number):Promise<Outcome<null>> };

export function secretBytes(secret:unknown): Uint8Array | null {
  if (typeof secret !== 'string' || !/^whsec_(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(secret)) return null;
  try { const raw = atob(secret.slice(6)); return raw.length >= 24 && raw.length <= 64 ? Uint8Array.from(raw, c => c.charCodeAt(0)) : null; } catch { return null; }
}
export async function sign(secret:string, id:string, timestamp:string, body:string):Promise<string> {
  const bytes = secretBytes(secret); if (!bytes) throw new Error('invalid secret');
  const key = await crypto.subtle.importKey('raw', bytes, {name:'HMAC',hash:'SHA-256'}, false, ['sign']);
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC',key,new TextEncoder().encode(`${id}.${timestamp}.${body}`)));
  return 'v1,' + btoa(String.fromCharCode(...mac));
}
export function destination(url:string, allowlist:string | undefined):Outcome<URL> {
  let parsed:URL; try { parsed = new URL(url); } catch { return {ok:false,error:'invalid_params'}; }
  if (parsed.protocol !== 'https:') return {ok:false,error:'invalid_params'};
  const host = parsed.hostname.replace(/^\[|\]$/g,'');
  if (parsed.username || parsed.password || ipaddr.isValid(host) || parsed.port || !allowlist?.split(',').map(x=>x.trim()).includes(parsed.origin)) return {ok:false,error:'callback_endpoint'};
  return {ok:true,value:parsed};
}
function publicAddress(raw:string):boolean {
  if (!ipaddr.isValid(raw)) return false;
  let addr = ipaddr.parse(raw);
  if (addr.kind() === 'ipv6' && (addr as ipaddr.IPv6).isIPv4MappedAddress()) addr = (addr as ipaddr.IPv6).toIPv4Address();
  if (addr.range() !== 'unicast') return false;
  if (addr.kind() === 'ipv4') return !(addr as ipaddr.IPv4).match(ipaddr.IPv4.parse('198.18.0.0'),15);
  const v6 = addr as ipaddr.IPv6;
  return v6.match(ipaddr.IPv6.parse('2000::'),3) && !v6.match(ipaddr.IPv6.parse('2001::'),23) && !v6.match(ipaddr.IPv6.parse('3fff::'),20);
}
export const dohResolver = (request: typeof fetch = fetch):Resolver => async (host, signal) => {
  const query = async (type:1 | 28):Promise<{a:string[];aaaa:string[]}> => {
    const endpoint = new URL('https://cloudflare-dns.com/dns-query');
    endpoint.searchParams.set('name',host);
    endpoint.searchParams.set('type',type === 1 ? 'A' : 'AAAA');
    const response = await request(endpoint.toString(), {headers:{accept:'application/dns-json'},redirect:'manual',signal});
    if (!response.ok) throw new Error('DNS request failed');
    const data:unknown = await response.json();
    if (!data || typeof data !== 'object') throw new Error('Malformed DNS response');
    const result = data as {Status?:unknown; TC?:unknown; Answer?:unknown};
    if (result.Status !== 0 || (result.TC !== undefined && result.TC !== false) || (result.Answer !== undefined && !Array.isArray(result.Answer))) throw new Error('DNS resolution failed');
    const addresses:{a:string[];aaaa:string[]} = {a:[],aaaa:[]};
    const records = result.Answer ?? [];
    for (const record of records) {
      if (!record || typeof record !== 'object' || typeof record.type !== 'number' || typeof record.data !== 'string' || typeof record.name !== 'string') throw new Error('Malformed DNS answer');
    }
    const normalize = (name:string) => name.toLowerCase().replace(/\.$/,'');
    const names = new Set<string>();
    let current = normalize(host);
    for (;;) {
      if (names.has(current)) throw new Error('DNS alias loop');
      names.add(current);
      const aliases = records.filter(record => record.type === 5 && normalize(record.name) === current);
      if (!aliases.length) break;
      if (aliases.length !== 1 || !normalize(aliases[0].data)) throw new Error('Malformed DNS alias');
      current = normalize(aliases[0].data);
    }
    // Only follow aliases supplied in this answer; never make alias queries.
    for (const record of records) {
      if ((record.type === 1 || record.type === 28 || record.type === 5) && !names.has(normalize(record.name))) throw new Error('Unrelated DNS answer');
      if (record.type === 1) addresses.a.push(record.data);
      if (record.type === 28) addresses.aaaa.push(record.data);
    }
    return addresses;
  };
  const [v4, v6] = await Promise.all([query(1),query(28)]);
  return {a:[...v4.a,...v6.a],aaaa:[...v4.aaaa,...v6.aaaa]};
};
export const workersResolver:Resolver = dohResolver();
function failure(error:unknown):WebhookError {
  let e = error as {name?:string;message?:string;code?:string;cause?:unknown} | undefined;
  for (let i=0;e && i<4;i++,e=e.cause as typeof e) {
    if (e.name === 'AbortError' || e.name === 'TimeoutError' || e.code === 'ETIMEDOUT') return 'timeout';
    if (/tls|certificate|ssl/i.test(`${e.message ?? ''} ${e.code ?? ''}`)) return 'tls_error';
  }
  return 'connection_refused';
}
function equal(a:string,b:string):boolean {
  const x = new TextEncoder().encode(a), y = new TextEncoder().encode(b);
  let diff = x.length ^ y.length;
  for (let i=0;i<Math.max(x.length,y.length);i++) diff |= (x[i] ?? 0) ^ (y[i] ?? 0);
  return diff === 0;
}
export function safeHttps(allowlist:string | undefined, resolver:Resolver = workersResolver, transport:Transport = fetch):SafeHttps {
  return { async verify(url,secret,subscriptionId,nonce,messageId,now) {
    const dest = destination(url,allowlist); if (!dest.ok) return dest;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(),5000);
    try {
      let answer:{a:string[];aaaa:string[]};
      try {
        answer = await Promise.race([resolver(dest.value.hostname,controller.signal), new Promise<never>((_,reject) => controller.signal.addEventListener('abort',()=>reject(new DOMException('timeout','TimeoutError')),{once:true}))]);
      } catch (e) { return {ok:false,error:failure(e) === 'timeout' ? 'timeout' : 'resolution_failed'}; }
      if (!answer || !Array.isArray(answer.a) || !Array.isArray(answer.aaaa)) return {ok:false,error:'resolution_failed'};
      if (answer.a.some(a => !ipaddr.IPv4.isValid(a)) || answer.aaaa.some(a => !ipaddr.IPv6.isValid(a))) return {ok:false,error:'resolution_failed'};
      const addresses = [...answer.a,...answer.aaaa];
      if (!addresses.length) return {ok:false,error:'resolution_failed'};
      if (addresses.some(a=>!publicAddress(a))) return {ok:false,error:'callback_endpoint'};
      const body = JSON.stringify({type:'verification',challenge:nonce});
      const timestamp = String(Math.floor(now/1000));
      // Workers cannot pin challenge or delivery connections to the checked address.
      // Fresh DNS and the exact-origin allowlist leave a DNS rebinding risk.
      const response = await transport(url,{method:'POST',redirect:'manual',signal:controller.signal,headers:{'Content-Type':'application/json','webhook-id':messageId,'webhook-timestamp':timestamp,'webhook-signature':await sign(secret,messageId,timestamp,body),'X-MCP-Subscription-Id':subscriptionId},body});
      if (response.status < 200 || response.status >= 300) return {ok:false,error:'challenge_failed'};
      let echo:unknown;
      try {
        const reader = response.body?.getReader();
        if (!reader) return {ok:false,error:'challenge_failed'};
        let bytes = 0, text = '';
        const decoder = new TextDecoder();
        for (;;) {
          const part = await reader.read();
          if (part.done) break;
          bytes += part.value.byteLength;
          if (bytes > 4096) { await reader.cancel(); return {ok:false,error:'challenge_failed'}; }
          text += decoder.decode(part.value,{stream:true});
        }
        echo = (JSON.parse(text + decoder.decode()) as {challenge?:unknown}).challenge;
      } catch (e) { if (controller.signal.aborted) return {ok:false,error:'timeout'}; return {ok:false,error:'challenge_failed'}; }
      return typeof echo === 'string' && equal(echo,nonce) ? {ok:true,value:null} : {ok:false,error:'challenge_failed'};
    } catch (e) { return {ok:false,error:failure(e)}; } finally { clearTimeout(timer); }
  }};
}
