package builtin

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	globalmemorycommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/globalmemory"
	mcpcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/mcp"
	memoriescommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/memories"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/session"
	stopcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/stop"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/usermanagement"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
)

// Dependencies supplies the services used by built-in commands.
type Dependencies struct {
	Accounts     *accounts.Service
	Memory       *memory.Store
	GlobalMemory *global.Store
	Logger       *config.Logger
	Bootstrap    commands.Handler
	MCPStore     *mcpmanager.Store
	MCPManager   *mcpmanager.Manager
	Canceler     stopcommands.Canceler
}

// NewService creates the command service with the configured optional integrations.
func NewService(deps Dependencies) (*commands.Service, error) {
	if deps.Memory == nil {
		return nil, fmt.Errorf("user memory store is required for built-in commands")
	}
	help := &helpHandler{}
	if deps.Accounts != nil {
		help.auth = deps.Accounts
	}
	registrations := []commands.Command{{Handler: help}, {Handler: sessioncommands.New(deps.Memory)}}
	if deps.Accounts != nil {
		registrations = append(registrations, commands.Command{Handler: memoriescommands.New(deps.Accounts, deps.Memory)})
	}
	if deps.MCPStore != nil && deps.MCPManager != nil {
		registrations = append(registrations, commands.Command{Handler: mcpcommands.New(deps.MCPStore, deps.MCPManager, deps.Accounts)})
	}
	if deps.Canceler != nil {
		registrations = append(registrations, commands.Command{Handler: stopcommands.New(deps.Canceler, deps.Accounts, deps.Accounts)})
	}
	if deps.Bootstrap != nil {
		registrations = append(registrations, commands.Command{Handler: deps.Bootstrap})
	}
	for _, handler := range accountlinking.New(deps.Accounts) {
		registrations = append(registrations, commands.Command{Handler: handler})
	}
	for _, handler := range usermanagement.New(deps.Accounts) {
		registrations = append(registrations, commands.Command{Handler: handler, Middleware: []commands.Middleware{commands.RequireAdmin(deps.Accounts)}})
	}
	if deps.GlobalMemory != nil {
		registrations = append(registrations, commands.Command{Handler: globalmemorycommands.New(deps.GlobalMemory, deps.Logger), Middleware: []commands.Middleware{commands.RequireAdmin(deps.Accounts)}})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		return nil, err
	}
	help.commands = service
	return service, nil
}
