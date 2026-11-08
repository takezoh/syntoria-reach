import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { WebStandardStreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/webStandardStreamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";

import { failure, nodeBroker, type Reply, unknownOutcome } from "./broker";
import type { Nodes } from "./nodes";

// The MCP endpoint. list_nodes lists the nodes with the agents each one can
// run; every other tool forwards one Reach method, with the same arguments
// but `node`, to the node it names, which authorizes and runs it. The
// gateway decides nothing about the request beyond who may call it and which
// node it goes to.

const INSTRUCTIONS = `Reach runs work on the user's machines (nodes). Call list_nodes first: it lists the nodes, whether each one is online, and the agents each one can run, with the role of each. Choose the node and the agent for the work by their roles, and pass the node to every other tool: workspaces, sessions and operations belong to their node.

One workspace is one unit of work on a node: a folder of its own, with a note saying what the work is and a status (todo, in-progress, in-review, completed). Before starting, call list_workspaces on the node.
- Work already in progress: continue in its workspace.
- New work: create_workspace with a note that says what the work is, so that you or another AI can find it again. The workspace starts empty; put in it what the work needs (for example, have the agent clone a repository).
Keep the note current with update_workspace as the work moves on. When the work is done, set the status to in-review, or to completed when nothing is left; a completed workspace is removed after a week without activity.

To have an agent do a task in a workspace: start_agent_session with the workspace and one of the node's agents; on a node with the Orca server, the agent runs in a terminal of the workspace there, where the user can watch it, and otherwise as a process on the node. send_agent_prompt with the task, then call read_agent_output until state is "idle": the agent has finished its turn, and reply is its last message of the turn. A reply may be cut off (on Orca after 8,000 characters): reply_truncated says so, so ask the agent for the rest or a shorter answer. Send the next prompt to the same session for a follow-up, and stop_agent_session when done, which ends the agent and the session; a session outlives a disconnect, so get_agent_session tells whether it is still there. If state is "waiting_on_human", the agent asks the user something in Orca, such as a permission: tell the user, and wait.

Tools that change something take an operation_id that you choose, unique per action (for example "create-ws-docs-1"). If a call fails with result_unknown, the request may have run: call get_operation on the same node with the same operation_id, or resend the same call with the same operation_id. Never retry with a new operation_id. node_offline means the node is not connected and nothing ran.`;

const node = z.string().min(1).describe("Node ID from list_nodes.");
const operationId = z.string().min(1).max(128).describe("An ID you choose, unique per action. Resending the same call with the same ID never runs it twice.");
const workspace = z.string().describe("Workspace ID from create_workspace or list_workspaces, on the same node.");
const note = z.string().min(1).max(2000).describe("What the work is, for whoever continues it: the goal, and what is done or next.");
const sessionId = z.string().describe("Session ID from start_agent_session, on the same node.");

type Shape = Record<string, z.ZodType>;

// The tools that forward a Reach method to a node; each also takes `node`.
const nodeTools: { name: string; description: string; input: Shape; readOnly: boolean }[] = [
  {
    name: "list_capabilities",
    description: "Your permissions on the node, and the commands and agents (with their roles) you may run in its workspaces.",
    input: {},
    readOnly: true,
  },
  {
    name: "list_workspaces",
    description: "Every workspace on the node, with its note, status and path. Look here before creating a workspace: continue ongoing work in its workspace.",
    input: {},
    readOnly: true,
  },
  {
    name: "create_workspace",
    description: "Creates a workspace on the node for one new unit of work: an empty folder of its own named reach-<name> (on Orca, the project reach-<name>), with a note saying what the work is. Returns an operation whose result.workspace is the workspace ID.",
    input: {
      operation_id: operationId,
      name: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]{0,57}$/).describe("Short name for the work; Reach prepends reach-."),
      note,
    },
    readOnly: false,
  },
  {
    name: "update_workspace",
    description: "Changes a workspace's note, its status (todo, in-progress, in-review, completed), or both. A completed workspace is removed after a week without activity. Returns an operation whose result is the workspace.",
    input: {
      operation_id: operationId,
      workspace,
      note: note.optional(),
      status: z.string().optional().describe("todo, in-progress, in-review or completed."),
    },
    readOnly: false,
  },
  {
    name: "start_agent_session",
    description: "Starts one of the node's agents in the workspace and opens a session with it: on a node with the Orca server, in a new terminal of the workspace there; otherwise as a process on the node. A completed workspace is set back in progress. Returns, once the agent waits for its first prompt or 15 s passed, an operation whose result is the session: session_id, workspace, and state (\"idle\" when it waits for a prompt, \"working\" while it starts).",
    input: {
      operation_id: operationId,
      workspace,
      agent: z.string().describe("Agent ID from list_nodes or list_capabilities."),
    },
    readOnly: false,
  },
  {
    name: "send_agent_prompt",
    description: "Delivers `text` to the agent as a prompt; returns once the agent started its turn, without waiting for the reply. Send it only while the session's state is \"idle\"; otherwise it fails with invalid_state: during a turn, while the agent may be waiting for the user to answer it in Orca, such as a permission (tell the user), or once the agent quit. Fails with harness_unavailable if the agent did not start a turn (it may be waiting for input in Orca). `text` is a prompt in words, up to 64 KiB, without control characters other than newlines and tabs; it must not start with ! or /, which the agent would run as a shell command or one of its own commands. Read the reply with read_agent_output.",
    input: { operation_id: operationId, session_id: sessionId, text: z.string().min(1) },
    readOnly: false,
  },
  {
    name: "read_agent_output",
    description: "The agent's state and, once its turn has ended, its reply. state is idle (it waits for a prompt; reply is its last message of the turn, empty before the first prompt), working, waiting_on_human (it asks the user something in Orca; waiting_on_human says what Orca saw) or exited (the agent quit). reply is empty unless state is idle. reply_truncated is true when the reply reached the length the node keeps (8,000 characters on Orca) and its end may be cut off. A read while the agent works may take a few seconds. Once the session ended, it is gone: not_found.",
    input: { session_id: sessionId },
    readOnly: true,
  },
  {
    name: "get_agent_session",
    description: "The session as the node observes it: its workspace, its agent, and its state (idle, working, waiting_on_human or exited, as in read_agent_output). not_found once the session ended.",
    input: { session_id: sessionId },
    readOnly: true,
  },
  {
    name: "stop_agent_session",
    description: "Ends the agent and its session (on Orca, it closes the agent's terminal): its reply is no longer readable.",
    input: { operation_id: operationId, session_id: sessionId },
    readOnly: false,
  },
  {
    name: "get_operation",
    description: "The state and result of an operation you started on the node, by its operation_id. Use it when a call failed with result_unknown.",
    input: { operation_id: operationId },
    readOnly: true,
  },
];

export async function mcp(request: Request, env: Env, nodes: Nodes): Promise<Response> {
  const server = new McpServer({ name: "syntoria-reach", version: "0.2.0" }, { instructions: INSTRUCTIONS });
  server.registerTool(
    "list_nodes",
    {
      description: "The user's nodes: for each, its ID, whether it is online, and, when it is, the agents you may start there with the role of each (what the agent is for). Choose the node and agent for the work here; an offline node runs nothing.",
      inputSchema: {},
      annotations: { readOnlyHint: true },
    },
    () => listNodes(env, nodes),
  );
  for (const tool of nodeTools) {
    server.registerTool(
      tool.name,
      { description: tool.description, inputSchema: { node, ...tool.input }, annotations: { readOnlyHint: tool.readOnly } },
      ({ node: target, ...params }: Record<string, unknown>) => forward(env, nodes, String(target), tool.name, params),
    );
  }
  // Stateless: every request stands alone, so any Worker instance can serve it.
  const transport = new WebStandardStreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
  await server.connect(transport);
  return transport.handleRequest(request);
}

async function call(env: Env, node: string, method: string, params: Record<string, unknown>): Promise<Reply> {
  try {
    return await nodeBroker(env, node).call(method, params);
  } catch {
    return unknownOutcome("The gateway lost the request");
  }
}

async function forward(env: Env, nodes: Nodes, node: string, method: string, params: Record<string, unknown>): Promise<CallToolResult> {
  const reply = nodes.has(node)
    ? await call(env, node, method, params)
    : failure("not_found", `There is no node ${JSON.stringify(node)}. list_nodes lists the nodes. Nothing was run.`);
  if ("error" in reply) {
    return { isError: true, content: [{ type: "text", text: JSON.stringify({ error: reply.error }) }] };
  }
  return { content: [{ type: "text", text: JSON.stringify(reply.result) }] };
}

// listNodes asks every node, at once, for the agents the caller may start
// there. A node that is not connected is offline; one that does not answer
// is online with the error.
async function listNodes(env: Env, nodes: Nodes): Promise<CallToolResult> {
  const entries = await Promise.all(
    [...nodes.keys()].sort().map(async (id) => {
      const reply = await call(env, id, "list_capabilities", {});
      if ("error" in reply) {
        return reply.error.code === "node_offline" ? { node: id, online: false } : { node: id, online: true, error: reply.error };
      }
      const agents = (reply.result as { agents?: unknown } | null)?.agents ?? [];
      return { node: id, online: true, agents };
    }),
  );
  return { content: [{ type: "text", text: JSON.stringify({ nodes: entries }) }] };
}
