// Package localapi serves the Reach service over HTTP on a Unix socket.
//
// Every request is POST /v1/<method> with a JSON body and an
// "Authorization: Bearer <token>" header. The peer must run as the same
// user as the daemon (SO_PEERCRED); the token then selects the client and
// its permissions. Errors are {"error": {"code", "message"}}.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/takezoh/syntoria-reach/src/internal/service"
)

const maxBodySize = 16 << 20

type peerUIDKey struct{}

// Listen creates the socket at path, readable and writable only by the
// daemon's user. A stale socket file is replaced.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// NewServer returns an HTTP server for svc. Use it with a listener from Listen.
func NewServer(svc *service.Service) *http.Server {
	return &http.Server{
		Handler: &handler{svc: svc, uid: os.Getuid()},
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if uid, err := peerUID(c); err == nil {
				ctx = context.WithValue(ctx, peerUIDKey{}, uid)
			}
			return ctx
		},
	}
}

func peerUID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Uid), nil
}

type handler struct {
	svc *service.Service
	uid int
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &service.Error{Code: service.CodeInvalidRequest, Message: "use POST"})
		return
	}
	if uid, ok := r.Context().Value(peerUIDKey{}).(int); !ok || uid != h.uid {
		writeError(w, &service.Error{Code: service.CodeUnauthorized, Message: "peer user is not allowed"})
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeError(w, &service.Error{Code: service.CodeUnauthorized, Message: "missing bearer token"})
		return
	}
	client, err := h.svc.Authenticate(token)
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	var result any
	switch strings.TrimPrefix(r.URL.Path, "/v1/") {
	case "list_projects":
		result = h.svc.ListProjects(client)
	case "list_workspaces":
		result, err = h.svc.ListWorkspaces(client)
	case "create_workspace":
		result, err = call(r, func(req service.CreateWorkspaceRequest) (any, error) {
			return h.svc.CreateWorkspace(r.Context(), client, req)
		})
	case "delete_workspace":
		result, err = call(r, func(req service.WorkspaceRequest) (any, error) {
			return h.svc.DeleteWorkspace(r.Context(), client, req)
		})
	case "list_files":
		result, err = call(r, func(req service.ListFilesRequest) (any, error) { return h.svc.ListFiles(client, req) })
	case "read_file":
		result, err = call(r, func(req service.ReadFileRequest) (any, error) { return h.svc.ReadFile(client, req) })
	case "write_file":
		result, err = call(r, func(req service.WriteFileRequest) (any, error) { return h.svc.WriteFile(client, req) })
	case "run_command":
		result, err = call(r, func(req service.RunCommandRequest) (any, error) {
			return h.svc.RunCommand(r.Context(), client, req)
		})
	case "get_operation":
		result, err = call(r, func(req service.GetOperationRequest) (any, error) { return h.svc.GetOperation(client, req) })
	case "start_job":
		result, err = call(r, func(req service.StartJobRequest) (any, error) { return h.svc.StartJob(client, req) })
	case "get_job":
		result, err = call(r, func(req service.JobRequest) (any, error) { return h.svc.GetJob(client, req) })
	case "read_job_output":
		result, err = call(r, func(req service.ReadJobOutputRequest) (any, error) { return h.svc.ReadJobOutput(client, req) })
	case "cancel_job":
		result, err = call(r, func(req service.CancelJobRequest) (any, error) { return h.svc.CancelJob(client, req) })
	case "start_agent_session":
		result, err = call(r, func(req service.StartAgentSessionRequest) (any, error) {
			return h.svc.StartAgentSession(r.Context(), client, req)
		})
	case "send_agent_prompt":
		result, err = call(r, func(req service.SendAgentPromptRequest) (any, error) { return h.svc.SendAgentPrompt(client, req) })
	case "get_agent_session":
		result, err = call(r, func(req service.AgentSessionRequest) (any, error) { return h.svc.GetAgentSession(client, req) })
	case "read_agent_events":
		result, err = call(r, func(req service.ReadAgentEventsRequest) (any, error) { return h.svc.ReadAgentEvents(client, req) })
	case "stop_agent_session":
		result, err = call(r, func(req service.StopAgentSessionRequest) (any, error) { return h.svc.StopAgentSession(client, req) })
	default:
		err = &service.Error{Code: service.CodeNotFound, Message: "unknown method " + r.URL.Path}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func call[T any](r *http.Request, fn func(T) (any, error)) (any, error) {
	var req T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, &service.Error{Code: service.CodeInvalidRequest, Message: fmt.Sprintf("decode request: %v", err)}
	}
	return fn(req)
}

func writeError(w http.ResponseWriter, err error) {
	var e *service.Error
	if !errors.As(err, &e) {
		e = &service.Error{Code: service.CodeInternal, Message: "internal error"}
	}
	status := map[string]int{
		service.CodeInvalidRequest:    http.StatusBadRequest,
		service.CodeUnauthorized:      http.StatusUnauthorized,
		service.CodePermissionDenied:  http.StatusForbidden,
		service.CodeNotFound:          http.StatusNotFound,
		service.CodeOperationConflict: http.StatusConflict,
		service.CodeVersionConflict:   http.StatusConflict,
		service.CodeInvalidState:      http.StatusConflict,
	}[e.Code]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": e})
}
