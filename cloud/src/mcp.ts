import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { WebStandardStreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/webStandardStreamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";

import { nodeBroker, type Reply, unknownOutcome } from "./broker";

// The MCP endpoint. Each tool forwards one Reach method, with the same
// arguments, to the Linux node, which authorizes and runs it. The gateway
// decides nothing about the request beyond who may call it.

const INSTRUCTIONS = `Reach runs work on the user's Linux machine.

To have an agent do a task: list_projects, then create_workspace from a project (a Git worktree with its own branch), start_agent_session in it with one of the project's agents, send_agent_prompt with the task, and call read_agent_events (passing the last seq you saw as "after") until an event of type "turn_completed" arrives. Send the next prompt to the same session for a follow-up. stop_agent_session when done; delete_workspace removes the worktree once its changes are committed or no longer wanted.

Tools that change something take an operation_id that you choose, unique per action (for example "create-ws-docs-1"). If a call fails with result_unknown, the request may have run: call get_operation with the same operation_id, or resend the same call with the same operation_id. Never retry with a new operation_id. node_offline means the machine is not connected and nothing ran.`;

const operationId = z.string().min(1).max(128).describe("An ID you choose, unique per action. Resending the same call with the same ID never runs it twice.");
const workspace = z.string().describe("Workspace ID from create_workspace or list_workspaces.");
const sessionId = z.string().describe("Session ID from start_agent_session.");

type Shape = Record<string, z.ZodType>;

const tools: { name: string; description: string; input: Shape; readOnly: boolean }[] = [
  {
    name: "list_projects",
    description: "Projects you may create workspaces from, with your permissions, commands and agents.",
    input: {},
    readOnly: true,
  },
  {
    name: "list_workspaces",
    description: "Your workspaces, with their paths and branches.",
    input: {},
    readOnly: true,
  },
  {
    name: "create_workspace",
    description: "Creates a workspace: a Git worktree and branch named `name`, from the project's default base (e.g. origin/main). Returns an operation whose result.workspace is the workspace ID.",
    input: {
      operation_id: operationId,
      project: z.string().describe("Project ID from list_projects."),
      name: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/).describe("Worktree and branch name."),
    },
    readOnly: false,
  },
  {
    name: "delete_workspace",
    description: "Removes one of your workspaces (worktree and, if merged, branch). Refused while it has uncommitted changes, running jobs or active agent sessions.",
    input: { operation_id: operationId, workspace },
    readOnly: false,
  },
  {
    name: "start_agent_session",
    description: "Starts an agent harness in a workspace and opens a session. Returns an operation whose result.session_id is the session. Pass resume_session_id (one of your ended sessions in the same workspace and agent) to continue its conversation.",
    input: {
      operation_id: operationId,
      workspace,
      agent: z.string().describe("Agent ID from the project's agents in list_projects."),
      resume_session_id: z.string().optional(),
      model: z.string().optional().describe("Model for codex and claude agents."),
      effort: z.string().optional().describe("Reasoning effort for codex and claude agents."),
    },
    readOnly: false,
  },
  {
    name: "send_agent_prompt",
    description: "Starts one turn of the session with `text` and returns its turn number without waiting. Refused while a turn is running. Read the progress with read_agent_events.",
    input: { operation_id: operationId, session_id: sessionId, text: z.string().min(1) },
    readOnly: false,
  },
  {
    name: "read_agent_events",
    description: "Events of the session after sequence `after`, in order: message, error and turn_completed (its text is the turn status). Also returns the session state (idle, running, closed, failed, unknown).",
    input: {
      session_id: sessionId,
      after: z.number().int().min(0).optional().describe("The last seq you saw; 0 or omitted for the start."),
      limit: z.number().int().min(1).max(1000).optional(),
    },
    readOnly: true,
  },
  {
    name: "get_agent_session",
    description: "State of a session: idle, running, closed, failed or unknown, with its number of turns.",
    input: { session_id: sessionId },
    readOnly: true,
  },
  {
    name: "stop_agent_session",
    description: "Interrupts any running turn and ends the session's harness.",
    input: { operation_id: operationId, session_id: sessionId },
    readOnly: false,
  },
  {
    name: "get_operation",
    description: "The state and result of an operation you started, by its operation_id. Use it when a call failed with result_unknown.",
    input: { operation_id: operationId },
    readOnly: true,
  },
];

export async function mcp(request: Request, env: Env): Promise<Response> {
  const server = new McpServer({ name: "syntoria-reach", version: "0.1.0" }, { instructions: INSTRUCTIONS });
  for (const tool of tools) {
    server.registerTool(
      tool.name,
      { description: tool.description, inputSchema: tool.input, annotations: { readOnlyHint: tool.readOnly } },
      (args: Record<string, unknown>) => forward(env, tool.name, args),
    );
  }
  // Stateless: every request stands alone, so any Worker instance can serve it.
  const transport = new WebStandardStreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
  await server.connect(transport);
  return transport.handleRequest(request);
}

async function forward(env: Env, method: string, params: Record<string, unknown>): Promise<CallToolResult> {
  let reply: Reply;
  try {
    reply = await nodeBroker(env).call(method, params);
  } catch {
    reply = unknownOutcome("The gateway lost the request");
  }
  if ("error" in reply) {
    return { isError: true, content: [{ type: "text", text: JSON.stringify({ error: reply.error }) }] };
  }
  return { content: [{ type: "text", text: JSON.stringify(reply.result) }] };
}
