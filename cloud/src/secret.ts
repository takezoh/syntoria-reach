// Secrets (Gate's resource client secret, the node tokens) are usable only
// when long enough to resist guessing, and node tokens are compared in
// constant time.

const MIN_SECRET_LENGTH = 32;

export function usable(secret: string | undefined): secret is string {
  return typeof secret === "string" && secret.length >= MIN_SECRET_LENGTH;
}

export async function sameSecret(given: string, expected: string): Promise<boolean> {
  const digest = (s: string) => crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  const [a, b] = await Promise.all([digest(given), digest(expected)]);
  return crypto.subtle.timingSafeEqual(a, b);
}

export function bearer(request: Request): string | null {
  const header = request.headers.get("Authorization") ?? "";
  return header.startsWith("Bearer ") ? header.slice("Bearer ".length) : null;
}
