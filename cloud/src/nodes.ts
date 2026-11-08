import { sameSecret, usable } from "./secret";

// The nodes of this gateway: the Linux machines whose daemons connect to it.
// NODES, a Worker secret, is a JSON object of node IDs and their node tokens,
// such as {"desktop": "<token>", "build": "<token>"}. A node is registered by
// adding its ID with a token of its own, and loses its access when its entry
// is removed or its token changed. The daemon sends only its token; the
// gateway finds the node by it.

export type Nodes = Map<string, string>;

const NODE_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

// parseNodes reads NODES; null when it is unset or malformed: not an object,
// a bad node ID, a token too short to resist guessing, or a token shared by
// two nodes.
export function parseNodes(raw: string | undefined): Nodes | null {
  if (!raw) {
    return null;
  }
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }
  const nodes: Nodes = new Map();
  const tokens = new Set<string>();
  for (const [id, token] of Object.entries(value)) {
    if (!NODE_ID.test(id) || typeof token !== "string" || !usable(token) || tokens.has(token)) {
      return null;
    }
    nodes.set(id, token);
    tokens.add(token);
  }
  return nodes.size > 0 ? nodes : null;
}

// nodeOf returns the node whose token is token, or null. Every token is
// compared, in constant time.
export async function nodeOf(nodes: Nodes, token: string): Promise<string | null> {
  let found: string | null = null;
  for (const [id, expected] of nodes) {
    if (await sameSecret(token, expected)) {
      found = id;
    }
  }
  return found;
}
