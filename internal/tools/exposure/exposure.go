package exposure

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Exposure tracks tools exposed during a single agent request.
type Exposure struct {
	mcpTools      map[string]bool
	hiddenBuiltin map[string]bool
}

// NewExposure creates empty request-local tool exposure state.
func NewExposure() *Exposure {
	return &Exposure{
		mcpTools:      make(map[string]bool),
		hiddenBuiltin: make(map[string]bool),
	}
}

// HideBuiltins records builtin tools that must not be visible for this request.
func (e *Exposure) HideBuiltins(names ...string) {
	if e == nil {
		return
	}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			e.hiddenBuiltin[name] = true
		}
	}
}

// ExposeTools records MCP tools that should be visible for the active request.
func (e *Exposure) ExposeTools(names []string) {
	if e == nil {
		return
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		e.mcpTools[name] = true
	}
}

// Visibility returns the registry visibility represented by this exposure state.
func (e *Exposure) Visibility() registry.ToolVisibility {
	if e == nil {
		return registry.ToolVisibility{}
	}
	return registry.ToolVisibility{HiddenBuiltins: e.hiddenBuiltin}
}

// ExposedMCPTools returns a copy of request-local exposed MCP tool names.
func (e *Exposure) ExposedMCPTools() map[string]bool {
	if e == nil {
		return nil
	}
	out := make(map[string]bool, len(e.mcpTools))
	for name, exposed := range e.mcpTools {
		out[name] = exposed
	}
	return out
}
