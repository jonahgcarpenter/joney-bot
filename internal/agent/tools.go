package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// MCPProvider resolves request-scoped MCP tools for the active canonical user.
type MCPProvider interface {
	DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool
	ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string
	LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (mcp.ExecutionResult, bool, error)
	ToolPolicy(name string) governance.ToolPolicy
}

type offeredToolCatalog struct {
	Tools    []llm.Tool
	Policies map[string]governance.ToolPolicy
}

func (a *Agent) toolsForRequest(ctx context.Context, principal identity.Principal, exposure *exposure.Exposure, governor *governance.Governor) offeredToolCatalog {
	catalog := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}
	add := func(tool llm.Tool, policy governance.ToolPolicy) {
		name := tool.Function.Name
		if _, exists := catalog.Policies[name]; name == "" || exists {
			return
		}
		if governor != nil && governor.IsToolRetired(name, policy) {
			return
		}
		catalog.Tools = append(catalog.Tools, tool)
		catalog.Policies[name] = policy
	}
	for _, tool := range a.registry.LLMToolsForVisibility(exposure.Visibility()) {
		if policy, ok := a.registry.Policy(tool.Function.Name); ok {
			add(tool, policy)
		}
	}
	if a.mcpProvider == nil {
		return catalog
	}
	for _, tool := range a.mcpProvider.DiscoveryTools(ctx, principal) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	for _, tool := range a.mcpProvider.LLMTools(ctx, principal, exposure.ExposedMCPTools()) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	return catalog
}

func (a *Agent) executeTool(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposure *exposure.Exposure) (governance.Result, error) {
	if a.registry.HasHandler(name) {
		return a.registry.Execute(ctx, name, args)
	}
	if a.mcpProvider != nil {
		if result, handled, err := a.mcpProvider.Execute(ctx, principal, name, args, exposure.ExposedMCPTools()); handled {
			return result.Result, err
		}
	}
	return a.registry.Execute(ctx, name, args)
}

func normalizeToolCallIDs(message *llm.ChatMessage, iteration int) {
	if message == nil {
		return
	}
	reserved := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if id := strings.TrimSpace(call.ID); id != "" {
			reserved[id] = true
		}
	}
	used := make(map[string]bool, len(message.ToolCalls))
	for i := range message.ToolCalls {
		id := strings.TrimSpace(message.ToolCalls[i].ID)
		if id != "" && !used[id] {
			message.ToolCalls[i].ID = id
			used[id] = true
			continue
		}
		base := fmt.Sprintf("call_%d_%d", iteration, i+1)
		id = base
		for suffix := 2; reserved[id] || used[id]; suffix++ {
			id = fmt.Sprintf("%s_%d", base, suffix)
		}
		message.ToolCalls[i].ID = id
		used[id] = true
	}
}

func governanceResultText(reason string) string {
	switch reason {
	case governance.ReasonDuplicate:
		return "Tool call blocked: the same tool and arguments were already executed in this request. Use the existing result or try meaningfully different arguments."
	case governance.ReasonToolLimit:
		return "Tool call blocked: this tool reached its execution limit for the request. Continue with the available results."
	case governance.ReasonToolFailures:
		return "Tool call blocked: the tool failure limit was reached. Continue without retrying this tool."
	case governance.ReasonToolUnproductive:
		return "Tool call blocked: this tool returned too many unproductive results. Continue with the available information."
	case governance.ReasonGlobalLimit, governance.ReasonIterationLimit:
		return "Tool call blocked: the request tool budget was exhausted. Finish the answer using the available results."
	case governance.ReasonUnadvertised:
		return "Tool call blocked: this tool was not available for this model step. Use only currently available tools."
	default:
		return "Tool call blocked by request policy. Continue with the available information."
	}
}
