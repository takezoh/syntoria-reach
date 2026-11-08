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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/takezoh/syntoria-reach/src/internal/api"
	"github.com/takezoh/syntoria-reach/src/internal/core"
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
		writeError(w, &core.Error{Code: core.CodeInvalidRequest, Message: "use POST"})
		return
	}
	if uid, ok := r.Context().Value(peerUIDKey{}).(int); !ok || uid != h.uid {
		writeError(w, &core.Error{Code: core.CodeUnauthorized, Message: "peer user is not allowed"})
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		writeError(w, &core.Error{Code: core.CodeUnauthorized, Message: "missing bearer token"})
		return
	}
	client, err := h.svc.Authenticate(token)
	if err != nil {
		writeError(w, err)
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		writeError(w, &core.Error{Code: core.CodeNotFound, Message: "unknown method " + r.URL.Path})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		writeError(w, &core.Error{Code: core.CodeInvalidRequest, Message: fmt.Sprintf("read request: %v", err)})
		return
	}
	result, err := api.Call(r.Context(), h.svc, client, method, body)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func writeError(w http.ResponseWriter, err error) {
	var e *core.Error
	if !errors.As(err, &e) {
		e = &core.Error{Code: core.CodeInternal, Message: "internal error"}
	}
	status := map[string]int{
		core.CodeInvalidRequest:    http.StatusBadRequest,
		core.CodeUnauthorized:      http.StatusUnauthorized,
		core.CodePermissionDenied:  http.StatusForbidden,
		core.CodeNotFound:          http.StatusNotFound,
		core.CodeOperationConflict: http.StatusConflict,
		core.CodeVersionConflict:   http.StatusConflict,
		core.CodeInvalidState:      http.StatusConflict,
	}[e.Code]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": e})
}
