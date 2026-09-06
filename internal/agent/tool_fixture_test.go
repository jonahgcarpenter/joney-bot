package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func registerTestTool(t *testing.T, reg *registry.Registry, spec registry.Spec, policy governance.ToolPolicy, handler registry.Handler) error {
	t.Helper()
	schema := llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{}}
	if spec.Schema != nil {
		schema = *spec.Schema
	} else {
		for _, param := range spec.Parameters {
			schema.Properties[param.Name] = llm.ToolParameterProperty{Type: param.Type, Description: param.Description, Enum: param.Enum}
			if param.Required {
				schema.Required = append(schema.Required, param.Name)
			}
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]llm.ToolParameterProperty{}
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	description := spec.Description
	if description == "" {
		description = spec.Name
	}
	definition := fmt.Sprintf("# %s\n\n## Description\n\n%s\n\n## Parameters\n\n| Name | Type | Required | Description |\n| ---- | ---- | -------- | ----------- |\n\n## Schema\n\n%s\n", spec.Name, description, encoded)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.md"), []byte(definition), 0o600); err != nil {
		return err
	}
	if err := reg.LoadFromDirectory(dir); err != nil {
		return err
	}
	return reg.RegisterHandler(spec.Name, policy, handler)
}
