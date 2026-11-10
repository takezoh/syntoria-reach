import { createRemoteJWKSet, errors, jwtVerify } from "jose";

export interface AccessConfig {
  teamDomain: string;
  audience: string;
  ownerEmail: string;
}

export class AccessError extends Error {
  constructor(readonly status: 401 | 403 | 503) {
    super(status === 503 ? "Access verification unavailable" : "Access denied");
  }
}

// The issuer is operator configuration, never a claim or URL from the request.
// Loopback HTTP exists solely for the local workerd acceptance harness.
export function accessIssuer(value: string): string {
  const url = new URL(value);
  const local = url.protocol === "http:" && ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
  if ((!local && url.protocol !== "https:") || url.username || url.password || url.search || url.hash ||
      url.pathname !== "/" || value !== url.origin) {
    throw new TypeError("ACCESS_TEAM_DOMAIN must be an HTTPS origin without a trailing slash");
  }
  return url.origin;
}

export function createAccessVerifier(config: AccessConfig) {
  const issuer = accessIssuer(config.teamDomain);
  if (![config.audience, config.ownerEmail].every((s) => typeof s === "string" && s.length > 0 && s.length <= 4096 && !/\s/.test(s))) {
    throw new TypeError("Access audience and owner email are required");
  }
  if (!/^[^@\s]+@[^@\s]+$/.test(config.ownerEmail)) throw new TypeError("Invalid Access owner email");
  const keys = createRemoteJWKSet(new URL(`${issuer}/cdn-cgi/access/certs`), { timeoutDuration: 5000 });

  return {
    async authenticate(assertion: string | null): Promise<void> {
      if (!assertion || assertion.length > 16_384) throw new AccessError(401);
      try {
        const { payload } = await jwtVerify(assertion, keys, {
          issuer,
          audience: config.audience,
          algorithms: ["RS256"],
          requiredClaims: ["sub", "exp", "iat"],
        });
        if (typeof payload.sub !== "string" || !payload.sub || payload.email !== config.ownerEmail) throw new AccessError(403);
      } catch (error) {
        if (error instanceof AccessError) throw error;
        if (error instanceof errors.JWTInvalid || error instanceof errors.JWTExpired ||
            error instanceof errors.JWTClaimValidationFailed || error instanceof errors.JWSInvalid ||
            error instanceof errors.JWSSignatureVerificationFailed || error instanceof errors.JOSEAlgNotAllowed ||
            error instanceof errors.JWKSNoMatchingKey || error instanceof errors.JOSENotSupported) {
          throw new AccessError(401);
        }
        // Network, timeout and malformed key-set failures are outages, not sign-outs.
        throw new AccessError(503);
      }
    },
  };
}
