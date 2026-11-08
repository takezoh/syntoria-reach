import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { WebStandardStreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/webStandardStreamableHttp.js";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";

import { nodeBroker, type Reply, unknownOutcome } from "./broker";

// The MCP endpoint. Each tool forwards one Reach method, with the same
// arguments, to the Linux node, which authorizes and runs it. The gateway
// decides nothing about the request beyond who may call it.

const INSTRUCTIONS = `Reach runs work on the user's Linux machine, in workspaces kept by the Orca server there.

One workspace is one unit of work: a folder of its own, with a note saying what the work is and a status (todo, in-progress, in-review, completed). Before starting, call list_workspaces.
- Work already in progress: continue in its workspace.
- New work: create_workspace with a note that says what the work is, so that you or another AI can find it again. The workspace starts empty; put in it what the work needs (for example, clone a repository).
Keep the note current with update_workspace as the work moves on. When the work is done, set the status to in-review, or to completed when nothing is left; a completed workspace is removed after a week without activity.

To have an agent do a task in a workspace: start_agent_session with one of your agents (list_capabilities); the agent runs in a terminal of the workspace on the Orca server, where the user can watch it. send_agent_prompt with the task, then call read_agent_output, passing the cursor it returned last time, until state is "idle": the agent has finished its turn and its output has been read. The output is the terminal's text, with the agent's status lines among it. Send the next prompt to the same session for a follow-up, and stop_agent_session when done, which closes the terminal and ends the session; a session outlives a disconnect, so get_agent_session tells whether it is still there. If state is "waiting_on_human", the agent asks the user something in Orca, such as a permission: tell the user, and wait.

Tools that change something take an operation_id that you choose, unique per action (for example "create-ws-docs-1"). If a call fails with result_unknown, the request may have run: call get_operation with the same operation_id, or resend the same call with the same operation_id. Never retry with a new operation_id. node_offline means the machine is not connected and nothing ran.`;

const operationId = z.string().min(1).max(128).describe("An ID you choose, unique per action. Resending the same call with the same ID never runs it twice.");
const workspace = z.string().describe("Workspace ID from create_workspace or list_workspaces.");
const note = z.string().min(1).max(2000).describe("What the work is, for whoever continues it: the goal, and what is done or next.");
const sessionId = z.string().describe("Session ID from start_agent_session.");

type Shape = Record<string, z.ZodType>;

const tools: { name: string; description: string; input: Shape; readOnly: boolean }[] = [
  {
    name: "list_capabilities",
    description: "Your permissions, and the commands and agents you may run in a workspace.",
    input: {},
    readOnly: true,
  },
  {
    name: "list_workspaces",
    description: "Every workspace, with its note, status and path. Look here before creating a workspace: continue ongoing work in its workspace.",
    input: {},
    readOnly: true,
  },
  {
    name: "create_workspace",
    description: "Creates a workspace for one new unit of work: an empty folder of its own, kept by Orca as the project reach-<name>, with a note saying what the work is. Returns an operation whose result.workspace is the workspace ID.",
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
    description: "Starts an agent in a new terminal of the workspace on the Orca server and opens a session with it; a completed workspace is set back in progress. Returns, once the agent waits for its first prompt or 15 s passed, an operation whose result is the session: session_id, and state (\"idle\" when it waits for a prompt).",
    input: {
      operation_id: operationId,
      workspace,
      agent: z.string().describe("Agent ID from list_capabilities."),
    },
    readOnly: false,
  },
  {
    name: "send_agent_prompt",
    description: "Types `text` into the agent's terminal and submits it; returns once the agent started its turn, without waiting for the reply. Send it only while the session's state is \"idle\"; otherwise it fails with invalid_state: during a turn, while the agent may be waiting for the user to answer it in Orca, such as a permission (tell the user), or once the agent quit. Fails with harness_unavailable if the agent did not start a turn (it may be waiting for input in Orca). `text` is a prompt in words, up to 64 KiB, without control characters other than newlines and tabs; it must not start with ! or /, which the agent would run as a shell command or one of its own commands. Read the reply with read_agent_output.",
    input: { operation_id: operationId, session_id: sessionId, text: z.string().min(1) },
    readOnly: false,
  },
  {
    name: "read_agent_output",
    description: "The session's terminal output after `cursor`, as Orca reads it: lines of text without escape sequences, including the prompts and the agent's status lines. Returns the cursor to pass next time, more (true when more lines follow: read again at once), truncated (lines before the cursor are no longer kept), and the agent's state: idle (it waits for a prompt; the output of its turn has been written), working, waiting_on_human (it asks the user something in Orca; waiting_on_human says what Orca saw) or exited (the agent quit; the terminal runs a shell). A read while the agent works takes a few seconds. Once the terminal closes, the session is gone: not_found.",
    input: {
      session_id: sessionId,
      cursor: z.string().optional().describe("The cursor from the last read; omitted for the oldest output Orca keeps."),
      limit: z.number().int().min(1).max(1000).optional().describe("At most this many lines."),
    },
    readOnly: true,
  },
  {
    name: "get_agent_session",
    description: "The session as Orca reports it: its workspace, the agent Orca sees in its terminal, and its state (idle, working, waiting_on_human or exited, as in read_agent_output). not_found once the terminal has closed.",
    input: { session_id: sessionId },
    readOnly: true,
  },
  {
    name: "stop_agent_session",
    description: "Closes the agent's terminal, which ends the agent and its session: the output is no longer readable.",
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
