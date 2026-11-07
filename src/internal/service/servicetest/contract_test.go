package servicetest

import (
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/service"
)

func TestStoreFake(t *testing.T) {
	StoreContract(t, func(*testing.T) service.Store { return NewStore() })
}

func TestFilesFake(t *testing.T) {
	FilesContract(t, func(t *testing.T) service.Files {
		f, err := (&Files{}).Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return f
	})
}
