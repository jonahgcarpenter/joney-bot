package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestRegistryLoadsMarkdownAndExecutesHandler(t *testing.T) {
	dir := t.TempDir()
	definition := `# test.echo

## Description

Echo a value.

## Parameters

| Name | Type | Required | Description |
| ---- | ---- | -------- | ----------- |
| text | string | yes | Text to echo |
`
	if err := os.WriteFile(filepath.Join(dir, "echo.md"), []byte(definition), 0o644); err != nil {
		t.Fatalf("write definition: %v", err)
	}

	reg, err := NewFromDirectory(dir, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if reg.Count() != 1 || reg.Names()[0] != "test.echo" {
		t.Fatalf("unexpected registry names: %+v", reg.Names())
	}
	if err := reg.RegisterHandler("missing", testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, nil
	}); err == nil {
		t.Fatal("expected unknown handler registration error")
	}
	if err := reg.RegisterHandler("test.echo", testToolPolicy(), func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: args["text"].(string), Outcome: governance.OutcomeProductive}, nil
	}); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	got, err := reg.Execute(context.Background(), "test.echo", map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.Content != "hello" || got.Outcome != governance.OutcomeProductive {
		t.Fatalf("got %+v, want productive hello result", got)
	}

	tools := reg.LLMTools()
	if len(tools) != 1 || tools[0].Function.Name != "test.echo" {
		t.Fatalf("unexpected LLM tools: %+v", tools)
	}
	if len(tools[0].Function.Parameters.Required) != 1 || tools[0].Function.Parameters.Required[0] != "text" {
		t.Fatalf("unexpected required params: %+v", tools[0].Function.Parameters.Required)
	}
}

func TestRegistryUnknownToolListsMatchingPrefixHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "files.search")
	registerTestTool(t, reg, "files.list")
	registerTestTool(t, reg, "files.delete")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "files.missing", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "files.missing"; available tools in prefix "files": files.delete, files.list, files.read, files.search`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func TestRegistryUnknownToolWithoutPrefixListsAllHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "files.delete")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "delete", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "delete"; available tools: files.delete, files.read, web.search`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func TestRegistryUnknownToolWithEmptyPrefixMatchListsNone(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "missing.write", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "missing.write"; available tools in prefix "missing": none`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func registerTestTool(t *testing.T, reg *Registry, name string) {
	t.Helper()
	reg.specs[name] = Spec{Name: name, Description: strings.TrimPrefix(name, "test.")}
	if err := reg.RegisterHandler(name, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "ok", Outcome: governance.OutcomeProductive}, nil
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func testToolPolicy() governance.ToolPolicy {
	return governance.ToolPolicy{MaxExecutions: 1, MaxFailures: 1, MaxUnproductive: 1}
}

func TestRegistryVisibilityAndOrdering(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	for _, spec := range []Spec{
		{Name: "test.second", Description: " Second "},
		{Name: "test.first", Description: " First "},
	} {
		reg.specs[spec.Name] = spec
	}

	tools := reg.LLMTools()
	if len(tools) != 2 || tools[0].Function.Name != "test.first" || tools[1].Function.Name != "test.second" {
		t.Fatalf("unexpected visible catalog order: %+v", tools)
	}
	if tools[0].Function.Description != "First" || tools[1].Function.Description != "Second" {
		t.Fatalf("descriptions were not trimmed: %+v", tools)
	}
	tools = reg.LLMToolsForVisibility(ToolVisibility{HiddenBuiltins: map[string]bool{"test.first": true}})
	if len(tools) != 1 || tools[0].Function.Name != "test.second" {
		t.Fatalf("request-hidden builtin remained visible: %+v", tools)
	}
}

func TestDisableBuiltinHidesToolButKeepsNameReserved(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	reg.specs["web.search"] = Spec{Name: "web.search", Description: "Search"}
	if err := reg.DisableBuiltin("web.search"); err != nil {
		t.Fatal(err)
	}
	if len(reg.LLMTools()) != 0 {
		t.Fatal("disabled builtin remained model-visible")
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "web.search" {
		t.Fatalf("reserved names = %v", names)
	}
	if len(reg.EnabledBuiltinNames()) != 0 {
		t.Fatalf("enabled builtins = %v", reg.EnabledBuiltinNames())
	}
}

func TestDisableBuiltinRejectsUnknownAndRegisteredTools(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	if err := reg.DisableBuiltin("missing"); err == nil {
		t.Fatal("unknown builtin was disabled")
	}
	registerTestTool(t, reg, "test.registered")
	if err := reg.DisableBuiltin("test.registered"); err == nil {
		t.Fatal("builtin was disabled after handler registration")
	}
}

func TestParseToolMarkdownRejectsMissingSections(t *testing.T) {
	if _, err := parseToolMarkdown("# missing.description\n\n## Parameters\n\n| Name | Type | Required | Description |\n| ---- | ---- | -------- | ----------- |"); err == nil {
		t.Fatal("expected missing description error")
	}
	if _, err := parseToolMarkdown("# missing.params\n\n## Description\n\nDescription"); err == nil {
		t.Fatal("expected missing parameters error")
	}
}

func TestRegistryValidatesMarkdownSchemaBeforeAdvertising(t *testing.T) {
	for _, test := range []struct {
		name, schema string
		valid        bool
	}{
		{name: "invalid JSON", schema: `{`},
		{name: "non-object", schema: `{"type":"array","properties":{}}`},
		{name: "missing properties", schema: `{"type":"object"}`},
		{name: "nested schema", schema: `{"type":"object","properties":{"items":{"type":"array","items":{"type":"string","enum":["a","b"]}}},"required":["items"],"additionalProperties":false}`, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			definition := "# test.schema\n\n## Description\n\nSchema test\n\n## Parameters\n\n| Name | Type | Required | Description |\n\n## Schema\n\n```json\n" + test.schema + "\n```\n"
			if err := os.WriteFile(filepath.Join(dir, "schema.md"), []byte(definition), 0o600); err != nil {
				t.Fatal(err)
			}
			reg, err := NewFromDirectory(dir, config.NewLogger(config.LevelError))
			if err != nil {
				t.Fatal(err)
			}
			tools := reg.LLMTools()
			if !test.valid {
				if len(tools) != 0 || reg.Count() != 0 {
					t.Fatalf("invalid schema loaded: %+v", tools)
				}
				return
			}
			if len(tools) != 1 {
				t.Fatalf("valid schema missing: %+v", tools)
			}
			params := tools[0].Function.Parameters
			items := params.Properties["items"]
			if params.AdditionalProperties == nil || *params.AdditionalProperties || len(params.Required) != 1 || params.Required[0] != "items" || items.Items == nil || items.Items.Type != "string" || len(items.Items.Enum) != 2 {
				t.Fatalf("schema constraints lost: %+v", params)
			}
		})
	}
}
