package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

const (
	maxDiscoveryResultChars = 16000
)

type discoveryData struct {
	Server string           `json:"server"`
	Query  string           `json:"query,omitempty"`
	Status string           `json:"status"`
	Tools  []discoveredTool `json:"tools"`
}

type discoveredTool struct {
	Name               string   `json:"name"`
	Server             string   `json:"server"`
	Description        string   `json:"description,omitempty"`
	RequiredParameters []string `json:"required_parameters,omitempty"`
}

// Provider exposes scoped MCP tools to the agent for a single request.
type Provider struct {
	manager       *Manager
	reservedTools map[string]bool
}

func NewProvider(manager *Manager, reservedToolNames ...string) *Provider {
	provider := &Provider{manager: manager, reservedTools: make(map[string]bool, len(reservedToolNames))}
	for _, name := range reservedToolNames {
		name = strings.TrimSpace(name)
		if name != "" {
			provider.reservedTools[name] = true
		}
	}
	return provider
}

func (p *Provider) DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool {
	if p == nil || p.manager == nil || p.manager.store == nil || !principal.Authenticated() {
		return nil
	}
	configs, err := p.manager.store.ListForUser(ctx, principal.CanonicalUserID)
	if err != nil {
		return nil
	}
	tools := make([]llm.Tool, 0, len(configs))
	for _, cfg := range configs {
		if !cfg.Enabled || strings.TrimSpace(cfg.Description) == "" || isReservedServerName(cfg.Name) {
			continue
		}
		tools = append(tools, discoveryTool(cfg.Name, cfg.Description))
	}
	return tools
}

// ResolveTools returns historical MCP tool names that remain available to the principal.
func (p *Provider) ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string {
	if p == nil || p.manager == nil || len(names) == 0 || !principal.Authenticated() {
		return nil
	}

	servers := make([]string, 0)
	seenServers := map[string]bool{}
	candidates := make([]string, 0, len(names))
	seenCandidates := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		server, remote, ok := splitToolName(name)
		if !ok || isReservedServerName(server) || strings.EqualFold(remote, "tools") || p.reservedTools[name] || seenCandidates[name] {
			continue
		}
		seenCandidates[name] = true
		candidates = append(candidates, name)
		if !seenServers[server] {
			seenServers[server] = true
			servers = append(servers, server)
		}
	}

	available := map[string]bool{}
	for _, server := range servers {
		specs, info, err := p.manager.ServerToolSpecs(ctx, principal.CanonicalUserID, server)
		if err != nil || strings.TrimSpace(info.Description) == "" || info.Status != serverStatusConnected {
			continue
		}
		for _, spec := range specs {
			available[spec.Name] = true
		}
	}

	resolved := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if available[name] {
			resolved = append(resolved, name)
		}
	}
	return resolved
}

func (p *Provider) LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool {
	if p == nil || p.manager == nil || len(exposed) == 0 || !principal.Authenticated() {
		return nil
	}
	specs := p.manager.ToolSpecs(ctx, principal.CanonicalUserID)
	tools := make([]llm.Tool, 0, len(specs))
	for _, spec := range specs {
		if isReservedServerName(spec.Server) || isReservedToolName(spec.Name) || p.reservedTools[spec.Name] || !exposed[spec.Name] {
			continue
		}
		tools = append(tools, llmTool(spec))
	}
	return tools
}

func (p *Provider) Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (ExecutionResult, bool, error) {
	if p == nil || p.manager == nil || !principal.Authenticated() || !strings.Contains(name, ".") {
		return ExecutionResult{}, false, nil
	}
	server, remote, ok := strings.Cut(name, ".")
	if !ok || isReservedServerName(server) {
		return ExecutionResult{}, false, nil
	}
	if remote == "tools" {
		result, err := p.discover(ctx, principal, server, args)
		return ExecutionResult{Result: result, ServerName: server, IsDiscovery: true}, true, err
	}
	if p.reservedTools[name] || !exposed[name] {
		return ExecutionResult{}, false, nil
	}
	if _, visible := p.manager.ServerInfo(ctx, principal.CanonicalUserID, server); !visible {
		return ExecutionResult{}, false, nil
	}
	result, err := p.manager.Execute(ctx, principal.CanonicalUserID, name, args)
	return result, true, err
}

func discoveryTool(server, description string) llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolDefinition{
		Name:        server + ".tools",
		Description: description,
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"query": {Type: "string", Description: "Optional case-insensitive search text. Matches tool names, descriptions, and parameter names/descriptions. Omit to return all tools from this server."},
			},
		},
	}}
}

func (p *Provider) discover(ctx context.Context, principal identity.Principal, server string, args map[string]interface{}) (governance.Result, error) {
	if isReservedServerName(server) {
		return governance.Result{Content: formatDiscoveryStatus(server, "", "not_found"), Outcome: governance.OutcomeUnproductive, ReasonCode: "not_found"}, nil
	}
	query := strings.TrimSpace(stringArg(args, "query"))
	entries, info, err := p.catalog(ctx, principal.CanonicalUserID, server)
	if err != nil {
		return governance.Result{}, fmt.Errorf("discover MCP server %q: %w", server, err)
	}
	if info.Status != serverStatusConnected {
		return governance.Result{}, fmt.Errorf("MCP server %q is unavailable", info.Name)
	}
	tools := searchTools(entries, info.Name, query)
	tools = filterReservedCatalog(tools, p.reservedTools)
	if len(tools) == 0 {
		return governance.Result{Content: formatDiscoveryResult(info.Name, query, tools), Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results"}, nil
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if exposer := requestctx.ToolExposerFromContext(ctx); exposer != nil {
		exposer.ExposeTools(names)
	}
	meta := requestctx.MetadataFromContext(ctx)
	if p.manager.log != nil {
		reqLog := p.manager.log.Agent("agent.tool.mcp.discovery", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
		reqLog.Debug("agent.tool.mcp.discovery", "listed MCP tools", config.F("server", info.Name), config.F("query_chars", len(query)), config.F("tool_count", len(tools)))
	}
	return governance.Result{Content: formatDiscoveryResult(info.Name, query, tools), Outcome: governance.OutcomeProductive}, nil
}

func filterReservedCatalog(entries []registry.CatalogEntry, reserved map[string]bool) []registry.CatalogEntry {
	if len(reserved) == 0 {
		return entries
	}
	filtered := make([]registry.CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if !reserved[entry.Name] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// ToolPolicy returns the request-governance policy for a dynamic MCP tool.
func (p *Provider) ToolPolicy(name string) governance.ToolPolicy {
	policy := governance.ToolPolicy{BlockDuplicates: true, History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
	if strings.HasSuffix(name, ".tools") {
		policy.NormalizeArgs = func(args map[string]interface{}) interface{} {
			query, _ := args["query"].(string)
			return map[string]interface{}{"query": strings.ToLower(strings.Join(strings.Fields(query), " "))}
		}
	}
	return policy
}

func (p *Provider) catalog(ctx context.Context, userID string, server string) ([]registry.CatalogEntry, ServerInfo, error) {
	if isReservedServerName(server) {
		return nil, ServerInfo{}, fmt.Errorf("MCP server name %q is reserved", server)
	}
	specs, info, err := p.manager.ServerToolSpecs(ctx, userID, server)
	if err != nil {
		return nil, info, err
	}
	entries := make([]registry.CatalogEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, registry.CatalogEntry{Name: spec.Name, Description: spec.Description, Source: registry.ToolSourceMCP, Server: spec.Server, Parameters: mapParams(spec.Parameters)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, info, nil
}

func llmTool(spec ToolSpec) llm.Tool {
	props := make(map[string]llm.ToolParameterProperty, len(spec.Parameters))
	required := []string{}
	for _, p := range spec.Parameters {
		description := p.Description
		if description == "" {
			description = "Input value for this MCP tool."
		}
		props[p.Name] = llm.ToolParameterProperty{Type: p.Type, Description: description, Enum: append([]string(nil), p.Enum...)}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	description := spec.Description
	if description == "" {
		description = "Execute this configured MCP tool."
	}
	return llm.Tool{Type: "function", Function: llm.ToolDefinition{Name: spec.Name, Description: description, Parameters: llm.ToolParameters{Type: "object", Properties: props, Required: required}}}
}

func mapParams(params []ParamSpec) []registry.ParamSpec {
	out := make([]registry.ParamSpec, 0, len(params))
	for _, p := range params {
		out = append(out, registry.ParamSpec{Name: p.Name, Type: p.Type, Required: p.Required, Description: p.Description, Enum: p.Enum})
	}
	return out
}

func searchTools(catalog []registry.CatalogEntry, server, query string) []registry.CatalogEntry {
	server = strings.TrimSpace(strings.ToLower(server))
	query = strings.TrimSpace(strings.ToLower(query))
	type scoredEntry struct {
		entry registry.CatalogEntry
		score int
	}
	var matches []scoredEntry
	for _, tool := range catalog {
		if server != "" && strings.ToLower(tool.Server) != server {
			continue
		}
		score := toolSearchScore(tool, query)
		if query != "" && score == 0 {
			continue
		}
		matches = append(matches, scoredEntry{entry: tool, score: score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].entry.Name < matches[j].entry.Name
	})
	tools := make([]registry.CatalogEntry, 0, len(matches))
	for _, match := range matches {
		tools = append(tools, match.entry)
	}
	return tools
}

func toolSearchScore(tool registry.CatalogEntry, query string) int {
	if query == "" {
		return 1
	}
	score := 0
	name := strings.ToLower(tool.Name)
	server := strings.ToLower(tool.Server)
	description := strings.ToLower(tool.Description)
	if strings.Contains(name, query) {
		score += 100
	}
	if strings.Contains(server, query) {
		score += 50
	}
	if strings.Contains(description, query) {
		score += 25
	}
	for _, term := range strings.Fields(query) {
		if strings.Contains(name, term) {
			score += 20
		}
		if strings.Contains(description, term) {
			score += 10
		}
		for _, param := range tool.Parameters {
			if strings.Contains(strings.ToLower(param.Name), term) {
				score += 8
			}
			if strings.Contains(strings.ToLower(param.Description), term) {
				score += 4
			}
		}
	}
	return score
}

func formatDiscoveryResult(server, query string, tools []registry.CatalogEntry) string {
	data := discoveryData{Server: server, Query: query, Status: "ok", Tools: make([]discoveredTool, 0, len(tools))}
	for _, tool := range tools {
		data.Tools = append(data.Tools, discoveredTool{
			Name: tool.Name, Server: tool.Server, Description: tool.Description,
			RequiredParameters: requiredParams(tool.Parameters),
		})
	}
	return encodeDiscoveryEnvelope(data)
}

func formatDiscoveryStatus(server, query, status string) string {
	return encodeDiscoveryEnvelope(discoveryData{
		Server: server, Query: query, Status: status, Tools: []discoveredTool{},
	})
}

func encodeDiscoveryEnvelope(data discoveryData) string {
	envelope := untrustedEnvelope{Type: "mcp_tool_discovery", Untrusted: true, Notice: untrustedDataNotice, Data: data}
	if encoded, err := json.Marshal(envelope); err == nil && len([]rune(string(encoded))) <= maxDiscoveryResultChars {
		return string(encoded)
	}

	allTools := data.Tools
	data.Tools = make([]discoveredTool, 0, len(allTools))
	envelope.Truncated = true
	envelope.Data = data
	if encoded, _ := json.Marshal(envelope); len([]rune(string(encoded))) > maxDiscoveryResultChars {
		runes := []rune(data.Query)
		low, high := 0, len(runes)
		for low <= high {
			mid := low + (high-low)/2
			data.Query = string(runes[:mid])
			envelope.Data = data
			encoded, _ = json.Marshal(envelope)
			if len([]rune(string(encoded))) <= maxDiscoveryResultChars {
				low = mid + 1
			} else {
				high = mid - 1
			}
		}
		data.Query = string(runes[:high])
	}
	for _, tool := range allTools {
		data.Tools = append(data.Tools, tool)
		envelope.Data = data
		encoded, err := json.Marshal(envelope)
		if err != nil || len([]rune(string(encoded))) > maxDiscoveryResultChars {
			data.Tools = data.Tools[:len(data.Tools)-1]
			break
		}
	}
	envelope.Data = data
	encoded, _ := json.Marshal(envelope)
	return string(encoded)
}

func requiredParams(params []registry.ParamSpec) []string {
	var required []string
	for _, param := range params {
		if param.Required {
			required = append(required, param.Name)
		}
	}
	sort.Strings(required)
	return required
}

func stringArg(args map[string]interface{}, name string) string {
	value, _ := args[name].(string)
	return value
}
