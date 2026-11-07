// Command reach is the Syntoria Reach daemon. It serves the local API on a
// Unix socket and keeps operations and audit records in SQLite. It is the
// composition root: the only package that wires the adapters to the
// service.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent"
	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/ids"
	"github.com/takezoh/syntoria-reach/src/internal/job"
	"github.com/takezoh/syntoria-reach/src/internal/localapi"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/store"
	"github.com/takezoh/syntoria-reach/src/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		slog.Error("reach stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", defaultConfigPath(), "path to the config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()
	if n, err := st.MarkRunningUnknown(); err != nil {
		return err
	} else if n > 0 {
		slog.Warn("operations left running by a previous daemon are now unknown", "count", n)
	}
	svc := service.New(cfg, deps(cfg, st))
	defer svc.Close()

	l, err := localapi.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	srv := localapi.NewServer(svc)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	slog.Info("reach listening", "socket", cfg.Socket)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// deps wires the production adapters. Job output and harness logs are kept
// next to the database.
func deps(cfg *config.Config, st *store.Store) service.Deps {
	stateDir := filepath.Dir(cfg.Database)
	return service.Deps{
		Store: st,
		OpenFiles: func(dir string) (service.Files, error) {
			f, err := workspace.Open(dir)
			if err != nil {
				return nil, err // not a typed nil *workspace.FS
			}
			return f, nil
		},
		RunCommand: command.Run,
		Jobs:       job.NewRunner(filepath.Join(stateDir, "jobs")),
		Agents:     agent.New(filepath.Join(stateDir, "agents")),
		Orca:       &orca.CLI{Command: cfg.Orca},
		NewID:      ids.New,
	}
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(dir, "reach", "config.json")
}
