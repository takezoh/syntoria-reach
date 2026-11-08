// Verifies Syntoria Gate access tokens by online introspection: every
// request is checked with Gate, and no positive result is cached, so a
// connection revoked at Gate stops working at its next request.
//
// Adapted from packages/sdk/src/index.ts of the sibling syntoria-gate
// repository (@syntoria/gate-sdk 0.1.0, Apache-2.0, source SHA-256
// 38ad78304248372dd07f4116cf85a86e555921c7c2851cd50a51499e5bf2e0c1), keeping
// only bearer verification. Changes for Workers: fetch uses redirect
// "manual", because Workers has no "error" mode (a redirect is then refused as
// a non-2xx response); the Basic credentials are encoded with btoa; and loopback
// HTTP is allowed only when the issuer itself is a loopback HTTP origin, which
// only local tests configure.

export interface GatePrincipal {
  readonly issuer: string;
  readonly userId: string;
  readonly connectionId: string;
  readonly clientId: string;
}

export interface GateConfig {
  issuer: string;
  audience: string;
  clientId: string;
  clientSecret: string;
  timeoutMs?: number;
}

export interface GateVerifier {
  authenticate(header: string | null): Promise<GatePrincipal>;
}

// 401 is an invalid credential; 503 is Gate being unreachable or unusable, so
// an outage never looks like a sign-out.
export class GateError extends Error {
  readonly status: 401 | 503;
  readonly code: "invalid_token" | "authorization_unavailable";
  constructor(status: 401 | 503) {
    super(status === 401 ? "Invalid access token" : "Connection authorization unavailable");
    this.name = "GateError";
    this.status = status;
    this.code = status === 401 ? "invalid_token" : "authorization_unavailable";
  }
}

const MAX_RESPONSE_BYTES = 65_536;
const MAX_TOKEN_LENGTH = 16_384;
const bearerToken = /^[A-Za-z0-9\-._~+/]+=*(?![\s\S])/;
const nonempty = (value: unknown): value is string =>
  typeof value === "string" && value.length > 0 && value.length <= 4096 && !/[\u0000-\u001f\u007f]/u.test(value);
const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

function safeUrl(value: string, allowHttp: boolean): URL {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new TypeError("Invalid Gate endpoint URL");
  }
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.username || url.password || url.hash || url.search ||
      (url.protocol !== "https:" && !(allowHttp && loopback && url.protocol === "http:"))) {
    throw new TypeError("Gate endpoints require HTTPS without credentials, query or fragment");
  }
  return url;
}

function formEncode(value: string): string {
  return encodeURIComponent(value).replace(/[!'()*]/g, (char) => `%${char.charCodeAt(0).toString(16).toUpperCase()}`).replace(/%20/g, "+");
}

async function readJson(response: Response, signal: AbortSignal): Promise<Record<string, unknown>> {
  const type = response.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase();
  if (type !== "application/json") throw new GateError(503);
  const declaredLength = response.headers.get("content-length");
  if (declaredLength !== null && (!/^\d+$/.test(declaredLength) || Number(declaredLength) > MAX_RESPONSE_BYTES)) {
    await response.body?.cancel();
    throw new GateError(503);
  }
  if (!response.body) throw new GateError(503);
  const reader = response.body.getReader();
  const abortReader = () => {
    void reader.cancel().catch(() => undefined);
  };
  signal.addEventListener("abort", abortReader, { once: true });
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      signal.throwIfAborted();
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_RESPONSE_BYTES) throw new GateError(503);
      chunks.push(value);
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.byteLength;
    }
    const parsed: unknown = JSON.parse(new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(bytes));
    if (!object(parsed)) throw new GateError(503);
    return parsed;
  } finally {
    signal.removeEventListener("abort", abortReader);
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}

export function createGateVerifier(config: GateConfig): GateVerifier {
  const issuer = config.issuer;
  const allowHttp = issuer.startsWith("http://");
  const issuerUrl = safeUrl(issuer, allowHttp);
  if (![config.audience, config.clientId, config.clientSecret].every(nonempty)) throw new TypeError("Invalid Gate verifier configuration");
  const timeoutMs = config.timeoutMs ?? 3000;
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 60_000) throw new TypeError("Invalid Gate timeout");
  const audience = config.audience;
  const authorization = `Basic ${btoa(`${formEncode(config.clientId)}:${formEncode(config.clientSecret)}`)}`;
  let introspectionEndpoint: string | undefined;

  async function request(url: string, options: RequestInit, signal: AbortSignal) {
    const response = await fetch(url, { ...options, signal, redirect: "manual" });
    if (!response.ok || response.redirected || (response.url && response.url !== url)) {
      await response.body?.cancel();
      throw new GateError(503);
    }
    return readJson(response, signal);
  }

  async function discover(signal: AbortSignal) {
    if (introspectionEndpoint) return introspectionEndpoint;
    const metadata = await request(`${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`,
      { headers: { accept: "application/json" } }, signal);
    if (metadata.issuer !== issuer || !nonempty(metadata.introspection_endpoint)) throw new GateError(503);
    let endpoint: URL;
    try {
      endpoint = safeUrl(metadata.introspection_endpoint, allowHttp);
    } catch {
      throw new GateError(503);
    }
    if (endpoint.origin !== issuerUrl.origin) throw new GateError(503);
    introspectionEndpoint = endpoint.href;
    return introspectionEndpoint;
  }

  async function verifyToken(token: string): Promise<GatePrincipal> {
    if (token.length === 0 || token.length > MAX_TOKEN_LENGTH || !bearerToken.test(token)) {
      throw new GateError(401);
    }
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new GateError(503));
      }, timeoutMs);
    });
    async function authorize(): Promise<GatePrincipal> {
      const endpoint = await discover(controller.signal);
      const data = await request(endpoint, {
        method: "POST",
        headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded", authorization },
        body: new URLSearchParams({ token, token_type_hint: "access_token" }).toString(),
      }, controller.signal);
      if (typeof data.active !== "boolean") throw new GateError(503);
      if (!data.active) throw new GateError(401);
      const clientId = data.client_id;
      const authorizedParty = data.azp;
      if (data.token_type !== "Bearer" || data.gate_approved !== true || data.iss !== issuer || !nonempty(data.sub) || !nonempty(data.connection_id) ||
          !Number.isSafeInteger(data.exp) || (data.exp as number) <= Date.now() / 1000 ||
          !nonempty(clientId) || (authorizedParty !== undefined && (!nonempty(authorizedParty) || clientId !== authorizedParty))) throw new GateError(401);
      const audiences = typeof data.aud === "string" ? [data.aud] : data.aud;
      if (!Array.isArray(audiences) || audiences.length === 0 || !audiences.every(nonempty) || !audiences.includes(audience)) {
        throw new GateError(401);
      }
      return Object.freeze({ issuer, userId: data.sub, connectionId: data.connection_id, clientId });
    }
    try {
      return await Promise.race([authorize(), timeout]);
    } catch (error) {
      throw error instanceof GateError ? error : new GateError(503);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
      controller.abort();
    }
  }

  return {
    async authenticate(header) {
      if (typeof header !== "string") throw new GateError(401);
      const match = /^Bearer ([A-Za-z0-9\-._~+/]+=*)(?![\s\S])/i.exec(header);
      if (!match) throw new GateError(401);
      return verifyToken(match[1]!);
    },
  };
}
