package builtin

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/bootstrap"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func TestNewServiceAlwaysRegistersReset(t *testing.T) {
	memory := memorytest.NewStore(t, filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer memory.Close() // nolint:errcheck
	service, err := NewService(Dependencies{Memory: memory})
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

func TestNewServiceOptionalDependencies(t *testing.T) {
	if _, err := NewService(Dependencies{}); err == nil {
		t.Fatal("expected missing memory error")
	}
	memories := memorytest.NewStore(t, filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer memories.Close()
	for _, tc := range []struct {
		name string
		deps Dependencies
		want map[string]bool
	}{
		{name: "none"},
		{name: "MCP store only", deps: Dependencies{MCPStore: &mcp.Store{}}},
		{name: "MCP manager only", deps: Dependencies{MCPManager: &mcp.Manager{}}},
		{name: "MCP", deps: Dependencies{MCPStore: &mcp.Store{}, MCPManager: &mcp.Manager{}}, want: map[string]bool{"mcp": true}},
		{name: "canceler without MCP", deps: Dependencies{Canceler: &broker.Broker{}}, want: map[string]bool{"stop": true}},
		{name: "bootstrap", deps: Dependencies{Bootstrap: &bootstrap.Service{}}, want: map[string]bool{"bootstrap": true}},
		{name: "global memory", deps: Dependencies{GlobalMemory: &global.Store{}}, want: map[string]bool{"global-memory": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.deps.Memory = memories
			service, err := NewService(tc.deps)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"mcp", "stop", "bootstrap", "global-memory", "memories"} {
				if _, found := service.Definition(name); found != tc.want[name] {
					t.Errorf("command %q registered=%t, want %t", name, found, tc.want[name])
				}
			}
		})
	}
}

func TestNewServiceRegistersMemories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memory := memorytest.NewStore(t, path, log)
	defer memory.Close() // nolint:errcheck
	users := accounts.NewService(path, memory, nil, log)
	defer users.Close() // nolint:errcheck
	service, err := NewService(Dependencies{Accounts: users, Memory: memory})
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
