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

	"github.com/takezoh/syntoria-reach/src/internal/api"
	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/ids"
	"github.com/takezoh/syntoria-reach/src/internal/job"
	"github.com/takezoh/syntoria-reach/src/internal/localapi"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/remote"
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

	go cleanup(ctx, svc)

	connector := make(chan struct{})
	if r := cfg.Remote; r != nil {
		client, ok := svc.ClientByID(r.Client)
		if !ok {
			return errors.New("remote: unknown client " + r.Client) // config.Validate rules this out
		}
		c := &remote.Connector{URL: r.URL, Token: r.Token, Handler: func(ctx context.Context, method string, params []byte) (any, error) {
			return api.Call(ctx, svc, client, method, params)
		}}
		go func() { c.Run(ctx); close(connector) }()
	} else {
		close(connector)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	<-connector
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cleanupInterval is how often workspaces are checked for removal.
const cleanupInterval = time.Hour

// cleanup removes due completed workspaces, and the folders of projects
// removed in Orca, now and every cleanupInterval.
func cleanup(ctx context.Context, svc *service.Service) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		removed, err := svc.Cleanup(ctx, time.Now())
		for _, id := range removed {
			slog.Info("removed completed workspace", "workspace", id)
		}
		if err != nil {
			slog.Warn("workspace cleanup", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// deps wires the production adapters. Job output is kept next to the
// database.
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
		Orca:       &orca.CLI{Command: cfg.Orca},
		Folders:    workspace.Folders{},
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
