package builtin

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/testutil"
)

func TestNewServiceAlwaysRegistersReset(t *testing.T) {
	memory := testutil.NewMemoryStore(t, filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer memory.Close() // nolint:errcheck
	service, err := NewServiceWithGlobalMemory(nil, memory, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range service.Definitions() {
		if definition.Name == "reset" {
			return
		}
	}
	t.Fatal("reset command was not registered")
}

func TestNewServiceRegistersMemories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memory := testutil.NewMemoryStore(t, path, log)
	defer memory.Close() // nolint:errcheck
	users := accountlinking.NewService(path, memory, nil, log)
	defer users.Close() // nolint:errcheck
	service, err := NewServiceWithGlobalMemory(users, memory, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	foundMemories := false
	for _, definition := range service.Definitions() {
		if definition.Name == "memories" {
			foundMemories = true
		}
	}
	if !foundMemories {
		t.Fatal("memories command was not registered")
	}
}
