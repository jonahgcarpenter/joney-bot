package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// Handler is the execution function for a single tool.
// It receives the model's tool call arguments and returns typed productivity
// plus the content injected as a tool response. ctx propagates cancellation.
type Handler func(ctx context.Context, arguments map[string]interface{}) (governance.Result, error)

// ParamSpec describes one parameter row parsed from the markdown table.
type ParamSpec struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Enum        []string
}

// ToolSourceMCP identifies tools discovered from connected MCP servers.
const ToolSourceMCP = "mcp"

// Spec holds the fully parsed definition from a single tool markdown file.
// Name and Description are sent to the model via the LLM tool schema.
type Spec struct {
	Name        string
	Description string
	Parameters  []ParamSpec
	Schema      *llm.ToolParameters
}

// CatalogEntry is a registry tool definition annotated with its source.
type CatalogEntry struct {
	Name        string
	Description string
	Source      string
	Server      string
	Parameters  []ParamSpec
}

// ToolVisibility controls which builtin tools are hidden for the active request.
type ToolVisibility struct {
	HiddenBuiltins map[string]bool
}

// Registry maps tool names to their parsed Spec and registered Handler.
// Load tool definitions from a directory of markdown files, then register a Go
// handler for each tool before passing the registry to the agent.
type Registry struct {
	specs    map[string]Spec
	handlers map[string]Handler
	policies map[string]governance.ToolPolicy
	disabled map[string]bool
	log      *config.Logger
}

// New creates an empty Registry. Call LoadFromDirectory to populate
// it with tool definitions, then RegisterHandler for each tool that needs execution.
func New(log *config.Logger) *Registry {
	return &Registry{
		specs:    make(map[string]Spec),
		handlers: make(map[string]Handler),
		policies: make(map[string]governance.ToolPolicy),
		disabled: make(map[string]bool),
		log:      log,
	}
}

// DisableBuiltin keeps a builtin name reserved while removing it from model-visible catalogs.
func (r *Registry) DisableBuiltin(name string) error {
	_, ok := r.specs[name]
	if !ok {
		return fmt.Errorf("cannot disable builtin %q: no tool spec loaded with that name", name)
	}
	if _, ok := r.handlers[name]; ok {
		return fmt.Errorf("cannot disable builtin %q after registering its handler", name)
	}
	r.disabled[name] = true
	r.log.Debug("tool.registry.builtin_disabled", "disabled builtin tool", config.F("tool_name", name))
	return nil
}

// NewFromDirectory creates a Registry and loads tool definitions from dir.
func NewFromDirectory(dir string, log *config.Logger) (*Registry, error) {
	registry := New(log)
	if err := registry.LoadFromDirectory(dir); err != nil {
		return nil, fmt.Errorf("failed to load tool definitions: %w", err)
	}
	return registry, nil
}

// LoadFromDirectory reads all *.md files in dir and parses each as a tool definition.
// Files that fail to parse are logged and skipped; the method only returns an error
// if the directory itself cannot be read.
func (r *Registry) LoadFromDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read tools directory %q: %w", dir, err)
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			r.log.Warn("tool.registry.definition_read_failed", "failed to read tool definition", config.F("status", "degraded"), config.ErrorField(err))
			continue
		}

		spec, err := parseToolMarkdown(string(data))
		if err != nil {
			r.log.Warn("tool.registry.definition_parse_failed", "failed to parse tool definition", config.F("status", "degraded"), config.ErrorField(err))
			continue
		}

		r.specs[spec.Name] = spec
		r.log.Debug("tool.registry.definition_loaded", "loaded tool definition", config.F("tool_name", spec.Name), config.F("file", entry.Name()))
		loaded++
	}

	if loaded == 0 {
		r.log.Warn("tool.registry.empty", "no tool definitions found", config.F("path", dir), config.F("status", "degraded"))
	}

	return nil
}

// RegisterHandler associates a Handler with a tool name.
// Returns an error if the name does not match any loaded tool spec, to catch
// typos and orphaned handlers early at startup.
func (r *Registry) RegisterHandler(name string, policy governance.ToolPolicy, handler Handler) error {
	if _, ok := r.specs[name]; !ok {
		return fmt.Errorf("cannot register handler for %q: no tool spec loaded with that name", name)
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid policy for tool %q: %w", name, err)
	}
	r.handlers[name] = handler
	r.policies[name] = policy
	r.log.Debug("tool.registry.handler_registered", "registered tool handler", config.F("tool_name", name))
	return nil
}

// LLMTools converts builtin Specs into the []llm.Tool slice passed to ChatRequest.Tools.
func (r *Registry) LLMTools() []llm.Tool {
	return r.LLMToolsForVisibility(ToolVisibility{})
}

// LLMToolsForVisibility converts loaded Specs into the []llm.Tool slice passed to
// ChatRequest.Tools, excluding disabled and request-hidden builtins.
func (r *Registry) LLMToolsForVisibility(visibility ToolVisibility) []llm.Tool {
	tools := make([]llm.Tool, 0, len(r.specs))
	for _, spec := range r.orderedSpecs() {
		if r.disabled[spec.Name] || visibility.HiddenBuiltins[spec.Name] {
			continue
		}
		parameters := llm.ToolParameters{}
		if spec.Schema != nil {
			parameters = *spec.Schema
		} else {
			props := make(map[string]llm.ToolParameterProperty, len(spec.Parameters))
			required := []string{}
			for _, p := range spec.Parameters {
				props[p.Name] = llm.ToolParameterProperty{
					Type:        p.Type,
					Description: p.Description,
					Enum:        p.Enum,
				}
				if p.Required {
					required = append(required, p.Name)
				}
			}
			parameters = llm.ToolParameters{Type: "object", Properties: props, Required: required}
		}
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolDefinition{
				Name:        spec.Name,
				Description: strings.TrimSpace(spec.Description),
				Parameters:  parameters,
			},
		})
	}
	return tools
}

// EnabledBuiltinNames returns executable, model-visible builtin names in stable order.
func (r *Registry) EnabledBuiltinNames() []string {
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		if !r.disabled[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Execute calls the registered handler for the named tool with the given arguments.
// Returns an error if no handler is registered for the tool name.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (governance.Result, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return governance.Result{}, r.unknownToolError(name)
	}
	return handler(ctx, args)
}

// Policy returns the validated runtime governance policy for a builtin tool.
func (r *Registry) Policy(name string) (governance.ToolPolicy, bool) {
	policy, ok := r.policies[name]
	return policy, ok
}

func (r *Registry) unknownToolError(name string) error {
	if prefix, ok := toolPrefix(name); ok {
		return fmt.Errorf("no handler registered for tool %q; available tools in prefix %q: %s", name, prefix, strings.Join(r.handlerNamesWithPrefix(prefix), ", "))
	}
	return fmt.Errorf("no handler registered for tool %q; available tools: %s", name, strings.Join(r.handlerNames(), ", "))
}

func toolPrefix(name string) (string, bool) {
	prefix, _, ok := strings.Cut(name, ".")
	if !ok || prefix == "" {
		return "", false
	}
	return prefix, true
}

func (r *Registry) handlerNamesWithPrefix(prefix string) []string {
	names := make([]string, 0)
	needle := prefix + "."
	for name := range r.handlers {
		if strings.HasPrefix(name, needle) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

func (r *Registry) handlerNames() []string {
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// HasHandler returns true if a handler has been registered for the given tool name.
func (r *Registry) HasHandler(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Count returns the number of tool specs loaded in the registry.
func (r *Registry) Count() int {
	return len(r.specs)
}

// Names returns the loaded tool names in stable sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.specs))
	for name := range r.specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) orderedSpecs() []Spec {
	specs := make([]Spec, 0, len(r.specs))
	for _, spec := range r.specs {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		return specs[i].Name < specs[j].Name
	})
	return specs
}

// parseToolMarkdown parses a tool definition from a markdown string.
//
// Expected format:
//
//	# tool_name
//
//	## Description
//
//	Full description text (may include markdown formatting, lists, etc.)
//
//	## Parameters
//
//	| Name | Type | Required | Description |
//	|------|------|----------|-------------|
//	| param | string | yes | Description of the parameter |
func parseToolMarkdown(content string) (Spec, error) {
	var spec Spec

	lines := strings.Split(content, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			spec.Name = strings.TrimSpace(trimmed[2:])
			break
		}
	}
	if spec.Name == "" {
		return spec, fmt.Errorf("missing # heading for tool name")
	}

	sections := splitMarkdownSections(content)

	descSection, hasDesc := sections["Description"]
	if !hasDesc || strings.TrimSpace(descSection) == "" {
		return spec, fmt.Errorf("tool %q: missing ## Description section", spec.Name)
	}
	spec.Description = strings.TrimSpace(descSection)

	paramSection, hasParams := sections["Parameters"]
	if !hasParams || strings.TrimSpace(paramSection) == "" {
		return spec, fmt.Errorf("tool %q: missing ## Parameters section", spec.Name)
	}

	params, err := parseParameterTable(paramSection, spec.Name)
	if err != nil {
		return spec, err
	}
	spec.Parameters = params
	if schemaSection, ok := sections["Schema"]; ok && strings.TrimSpace(schemaSection) != "" {
		schemaText := strings.TrimSpace(schemaSection)
		schemaText = strings.TrimPrefix(schemaText, "```json")
		schemaText = strings.TrimPrefix(schemaText, "```")
		schemaText = strings.TrimSuffix(schemaText, "```")
		schemaText = strings.TrimSpace(schemaText)
		var schema llm.ToolParameters
		if err := json.Unmarshal([]byte(schemaText), &schema); err != nil {
			return spec, fmt.Errorf("tool %q: invalid ## Schema JSON: %w", spec.Name, err)
		}
		if schema.Type != "object" || schema.Properties == nil {
			return spec, fmt.Errorf("tool %q: ## Schema must describe an object with properties", spec.Name)
		}
		spec.Schema = &schema
	}

	return spec, nil
}

// splitMarkdownSections splits the content after the H1 heading into named
// sections keyed by their ## heading text. The value is the raw content between
// that heading and the next ## heading (or end of file).
func splitMarkdownSections(content string) map[string]string {
	sections := make(map[string]string)
	lines := strings.Split(content, "\n")

	currentSection := ""
	var sb strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}

		if strings.HasPrefix(trimmed, "## ") {
			if currentSection != "" {
				sections[currentSection] = sb.String()
				sb.Reset()
			}
			currentSection = strings.TrimSpace(trimmed[3:])
			continue
		}

		if currentSection != "" {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	if currentSection != "" {
		sections[currentSection] = sb.String()
	}

	return sections
}

// parseParameterTable parses a markdown table of tool parameters.
// Expected columns (in order): Name, Type, Required, Description.
// Skips the header row and any separator rows (containing only dashes and pipes).
// Zero-argument tools are allowed and return an empty parameter slice.
func parseParameterTable(section, toolName string) ([]ParamSpec, error) {
	var params []ParamSpec
	hasTableRow := false

	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		hasTableRow = true

		inner := strings.TrimPrefix(line, "|")
		inner = strings.TrimSuffix(inner, "|")
		cells := strings.Split(inner, "|")

		if len(cells) < 4 {
			continue
		}

		name := strings.TrimSpace(cells[0])
		typ := strings.TrimSpace(cells[1])
		reqStr := strings.TrimSpace(cells[2])
		desc := strings.TrimSpace(cells[3])

		if name == "Name" || strings.ContainsAny(name, "-") {
			continue
		}
		if name == "" || typ == "" {
			continue
		}

		params = append(params, ParamSpec{
			Name:        name,
			Type:        typ,
			Required:    strings.EqualFold(reqStr, "yes"),
			Description: desc,
		})
	}

	if !hasTableRow {
		return nil, fmt.Errorf("tool %q: parameter table has no valid rows", toolName)
	}

	return params, nil
}
