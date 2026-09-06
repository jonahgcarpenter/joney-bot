package tools

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The user memory store is created externally and shared with the tool handlers.
func NewRegistryFromConfig(cfg *config.Config, userMemStore *memory.Store, globalMemStore *global.Store, log *config.Logger) (*registry.Registry, error) {
	bootstrapLog := log.Server("tool.bootstrap")
	reg, err := registry.NewFromDirectory(config.DefaultToolsConfigDir, log.Server("tool.registry"))
	if err != nil {
		return nil, err
	}

	if err := builtin.Register(reg, cfg, userMemStore, globalMemStore, log); err != nil {
		return nil, err
	}

	enabled := reg.EnabledBuiltinNames()
	bootstrapLog.Info("tool.bootstrap.enabled", "enabled tools", config.F("tool_count", len(enabled)), config.F("tools", strings.Join(enabled, ",")))
	return reg, nil
}
