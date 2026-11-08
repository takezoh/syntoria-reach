package orcaws_test

// What Reach does with Orca, through the service, is covered by the service
// tests, which run on this backend; here the backend keeps the contract
// every workspace backend keeps.

import (
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/orcaws"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

func TestWorkspacesContract(t *testing.T) {
	servicetest.WorkspacesContract(t, func(t *testing.T) servicetest.Backend {
		f := servicetest.New()
		return servicetest.Backend{Workspaces: orcaws.New(f.Orca, f.Folders, "/w"), Folders: f.Folders, Dir: "/w"}
	})
}
