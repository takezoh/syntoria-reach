// Package api maps the Reach methods to the service. The local API (Unix
// socket) and the remote connector (Cloudflare gateway) serve the same
// methods with the same JSON requests and results; they differ only in how
// the caller is authenticated.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

// Call runs method as client c. body is the JSON request; methods without
// arguments ignore it. An unknown method is a not_found error.
func Call(ctx context.Context, svc *service.Service, c *service.Client, method string, body []byte) (any, error) {
	switch method {
	case "list_capabilities":
		return svc.ListCapabilities(c), nil
	case "list_workspaces":
		return svc.ListWorkspaces(ctx, c)
	case "create_workspace":
		return call(body, func(req service.CreateWorkspaceRequest) (any, error) { return svc.CreateWorkspace(ctx, c, req) })
	case "update_workspace":
		return call(body, func(req service.UpdateWorkspaceRequest) (any, error) { return svc.UpdateWorkspace(ctx, c, req) })
	case "list_files":
		return call(body, func(req service.ListFilesRequest) (any, error) { return svc.ListFiles(ctx, c, req) })
	case "read_file":
		return call(body, func(req service.ReadFileRequest) (any, error) { return svc.ReadFile(ctx, c, req) })
	case "write_file":
		return call(body, func(req service.WriteFileRequest) (any, error) { return svc.WriteFile(ctx, c, req) })
	case "run_command":
		return call(body, func(req service.RunCommandRequest) (any, error) { return svc.RunCommand(ctx, c, req) })
	case "get_operation":
		return call(body, func(req service.GetOperationRequest) (any, error) { return svc.GetOperation(c, req) })
	case "start_job":
		return call(body, func(req service.StartJobRequest) (any, error) { return svc.StartJob(ctx, c, req) })
	case "get_job":
		return call(body, func(req service.JobRequest) (any, error) { return svc.GetJob(c, req) })
	case "read_job_output":
		return call(body, func(req service.ReadJobOutputRequest) (any, error) { return svc.ReadJobOutput(c, req) })
	case "cancel_job":
		return call(body, func(req service.CancelJobRequest) (any, error) { return svc.CancelJob(c, req) })
	case "start_agent_session":
		return call(body, func(req service.StartAgentSessionRequest) (any, error) { return svc.StartAgentSession(ctx, c, req) })
	case "send_agent_prompt":
		return call(body, func(req service.SendAgentPromptRequest) (any, error) { return svc.SendAgentPrompt(c, req) })
	case "get_agent_session":
		return call(body, func(req service.AgentSessionRequest) (any, error) { return svc.GetAgentSession(c, req) })
	case "read_agent_events":
		return call(body, func(req service.ReadAgentEventsRequest) (any, error) { return svc.ReadAgentEvents(c, req) })
	case "stop_agent_session":
		return call(body, func(req service.StopAgentSessionRequest) (any, error) { return svc.StopAgentSession(c, req) })
	}
	return nil, &core.Error{Code: core.CodeNotFound, Message: "unknown method " + method}
}

func call[T any](body []byte, fn func(T) (any, error)) (any, error) {
	var req T
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, &core.Error{Code: core.CodeInvalidRequest, Message: fmt.Sprintf("decode request: %v", err)}
	}
	return fn(req)
}
