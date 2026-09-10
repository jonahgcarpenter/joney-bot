package builtin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func testConfig() *config.Config {
	return &config.Config{SearxngURL: "http://localhost:8080"}
}

func visibleTestTool(reg *registry.Registry, name string) (llm.Tool, bool) {
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == name {
			return tool, true
		}
	}
	return llm.Tool{}, false
}

func newTestRegistry(t *testing.T, log *config.Logger) *registry.Registry {
	t.Helper()
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	for _, name := range reg.Names() {
		if strings.HasPrefix(name, "soul.") {
			t.Fatalf("soul tool exposed in registry: %q", name)
		}
	}
	for _, tool := range reg.LLMTools() {
		if strings.HasPrefix(tool.Function.Name, "soul.") {
			t.Fatalf("soul tool advertised to model: %q", tool.Function.Name)
		}
	}
}

func TestRegisterIncludesCurrentTimeTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler("time.current") {
		t.Fatal("time.current handler was not registered")
	}

	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != "time.current" {
			continue
		}
		params := tool.Function.Parameters
		if len(params.Properties) != 1 || params.Properties["timezone"].Type != "string" || len(params.Required) != 1 || params.Required[0] != "timezone" {
			t.Fatalf("unexpected time.current parameters: %+v", params)
		}
		return
	}
	t.Fatal("time.current schema was not loaded")
}

func TestRegisterIncludesTranscriptSearchTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler(toolnames.SessionTranscriptSearch) {
		t.Fatalf("%s handler was not registered", toolnames.SessionTranscriptSearch)
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != toolnames.SessionTranscriptSearch {
			continue
		}
		params := tool.Function.Parameters
		if len(params.Properties) != 2 || params.Properties["query"].Type != "string" || params.Properties["limit"].Type != "integer" || len(params.Required) != 1 || params.Required[0] != "query" {
			t.Fatalf("unexpected %s parameters: %+v", toolnames.SessionTranscriptSearch, params)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.SessionTranscriptSearch)
}

func TestRegisterExposesUserMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolnames.UserMemorySave, toolnames.UserMemorySearch, toolnames.UserMemoryList, toolnames.SessionTranscriptSearch} {
		if _, ok := visibleTestTool(reg, name); !ok || !reg.HasHandler(name) {
			t.Fatalf("user memory tool is unavailable: %s", name)
		}
	}
}

func TestRegisterGlobalMemorySearchIsDefaultVisibleWithSchema(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	foundVisible := false
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == toolnames.GlobalMemorySearch {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatalf("%s is not default-visible", toolnames.GlobalMemorySearch)
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != toolnames.GlobalMemorySearch {
			continue
		}
		params := tool.Function.Parameters
		if len(params.Properties) != 2 || params.Properties["query"].Type != "string" || params.Properties["limit"].Type != "integer" || len(params.Required) != 1 || params.Required[0] != "query" {
			t.Fatalf("unexpected %s parameters: %+v", toolnames.GlobalMemorySearch, params)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.GlobalMemorySearch)
}

func TestRegisterCatalogOmitsRemovedGlobalMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	advertised := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		advertised[tool.Function.Name] = true
	}
	cataloged := map[string]bool{}
	for _, name := range reg.Names() {
		cataloged[name] = true
	}
	for _, name := range []string{"global_memory_save", "global_memory_list", "global_memory_forget"} {
		if advertised[name] || cataloged[name] || reg.HasHandler(name) {
			t.Fatalf("removed global memory tool is available: %s", name)
		}
	}
}

func TestRegisterAdvertisesFinalBuiltinToolNames(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"web.fetch":                       true,
		"web.search":                      true,
		"time.current":                    true,
		toolnames.UserMemorySearch:        true,
		toolnames.UserMemoryList:          true,
		toolnames.UserMemorySave:          true,
		toolnames.GlobalMemorySearch:      true,
		toolnames.SessionTranscriptSearch: true,
	}
	got := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		got[tool.Function.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("advertised tools = %#v, want %#v", got, want)
	}
	for name := range want {
		if !got[name] || !reg.HasHandler(name) {
			t.Fatalf("final builtin tool is unavailable: %s", name)
		}
	}
}

func TestRegisterUserMemorySavePolicyAndStrictSchema(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	policy, ok := reg.Policy(toolnames.UserMemorySave)
	if !ok || policy.MaxExecutions != 2 || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
		t.Fatalf("unexpected user memory save policy: %+v", policy)
	}
	tool, ok := visibleTestTool(reg, toolnames.UserMemorySave)
	if !ok {
		t.Fatal("user memory save schema is unavailable")
	}
	memories := tool.Function.Parameters.Properties["memories"]
	if tool.Function.Parameters.AdditionalProperties == nil || *tool.Function.Parameters.AdditionalProperties || memories.MinItems == nil || *memories.MinItems != 1 || memories.MaxItems == nil || *memories.MaxItems != 5 || memories.Items == nil || memories.Items.AdditionalProperties == nil || *memories.Items.AdditionalProperties {
		t.Fatalf("user memory save schema is not strict: %+v", tool.Function.Parameters)
	}
	for _, name := range []string{"supersedes", "evidence_type", "confidence", "reinforces_memory_id"} {
		if _, ok := memories.Items.Properties[name]; !ok {
			t.Fatalf("user memory save schema is missing %s", name)
		}
	}
	confidence := memories.Items.Properties["confidence"]
	reinforces := memories.Items.Properties["reinforces_memory_id"]
	if confidence.Minimum == nil || *confidence.Minimum != 0 || confidence.Maximum == nil || *confidence.Maximum != 1 || reinforces.Minimum == nil || *reinforces.Minimum != 0 {
		t.Fatalf("user memory assessment bounds are incomplete: confidence=%+v reinforces=%+v", confidence, reinforces)
	}
}

func TestNormalizeMemorySaveArgsAllowsCorrectiveRetryButBlocksExactDuplicate(t *testing.T) {
	base := map[string]interface{}{"memories": []interface{}{map[string]interface{}{
		"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
		"claim_slot": "preference.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": 0.9,
	}}}
	duplicate := map[string]interface{}{"memories": []interface{}{map[string]interface{}{
		"statement": " The user prefers tea. ", "evidence": "I prefer tea.", "category": "DURABLE_PREFERENCES",
		"claim_slot": "PREFERENCE.DRINK", "claim_value": "TEA", "supersedes": "", "evidence_type": "DIRECT_STATEMENT", "confidence": 0.9, "reinforces_memory_id": 0,
	}}}
	corrected := map[string]interface{}{"memories": []interface{}{map[string]interface{}{
		"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
		"claim_slot": "preference.favorite_drink", "claim_value": "tea", "supersedes": "", "evidence_type": "direct_statement", "confidence": 0.9,
	}}}
	baseJSON, _ := json.Marshal(normalizeMemorySaveArgs(base))
	duplicateJSON, _ := json.Marshal(normalizeMemorySaveArgs(duplicate))
	correctedJSON, _ := json.Marshal(normalizeMemorySaveArgs(corrected))
	if string(baseJSON) != string(duplicateJSON) {
		t.Fatalf("semantic duplicate normalized differently: %s != %s", baseJSON, duplicateJSON)
	}
	if string(baseJSON) == string(correctedJSON) {
		t.Fatalf("corrective retry normalized as duplicate: %s", correctedJSON)
	}
	changedAssessment := map[string]interface{}{"memories": []interface{}{map[string]interface{}{
		"statement": "The user prefers tea.", "evidence": "I prefer tea.", "category": "durable_preferences",
		"claim_slot": "preference.drink", "claim_value": "tea", "supersedes": "", "evidence_type": "model_inference", "confidence": 0.4, "reinforces_memory_id": 7,
	}}}
	changedJSON, _ := json.Marshal(normalizeMemorySaveArgs(changedAssessment))
	if string(baseJSON) == string(changedJSON) {
		t.Fatalf("different assessment normalized as duplicate: %s", changedJSON)
	}
}

func TestRegisterAdvertisesStrictWebSchemas(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	wantParameter := map[string]string{"web.fetch": "url", "web.search": "query"}
	for _, tool := range reg.LLMTools() {
		parameter, exists := wantParameter[tool.Function.Name]
		if !exists {
			continue
		}
		schema := tool.Function.Parameters
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Properties) != 1 || len(schema.Required) != 1 || schema.Required[0] != parameter {
			t.Fatalf("%s schema is not strict: %+v", tool.Function.Name, schema)
		}
		delete(wantParameter, tool.Function.Name)
	}
	if len(wantParameter) != 0 {
		t.Fatalf("web schemas were not advertised: %v", wantParameter)
	}
}

func TestRegisterRejectsInvalidSearxngURL(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{BraveAPIKey: "secret", SearxngURL: "localhost:8080"}, nil, nil, log); err == nil || !strings.Contains(err.Error(), "SearXNG web.search client") {
		t.Fatalf("invalid SearXNG URL registration error = %v", err)
	}
}

func TestRegisterWebSearchProviderMatrix(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		wantShown bool
	}{
		{name: "neither", cfg: &config.Config{}, wantShown: false},
		{name: "brave", cfg: &config.Config{BraveAPIKey: "secret"}, wantShown: true},
		{name: "searxng", cfg: &config.Config{SearxngURL: "http://localhost:8080"}, wantShown: true},
		{name: "both", cfg: &config.Config{BraveAPIKey: "secret", SearxngURL: "http://localhost:8080"}, wantShown: true},
		{name: "whitespace", cfg: &config.Config{BraveAPIKey: "  ", SearxngURL: "\t"}, wantShown: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := config.NewLogger(config.LevelError)
			reg := newTestRegistry(t, log)
			if err := Register(reg, test.cfg, nil, nil, log); err != nil {
				t.Fatal(err)
			}
			for _, toolName := range []string{"web.fetch", "web.search"} {
				_, shown := visibleTestTool(reg, toolName)
				if shown != test.wantShown || reg.HasHandler(toolName) != test.wantShown {
					t.Fatalf("%s shown=%t handler=%t", toolName, shown, reg.HasHandler(toolName))
				}
				if test.wantShown {
					policy, ok := reg.Policy(toolName)
					if !ok {
						t.Fatalf("missing %s policy", toolName)
					}
					if toolName == "web.search" && (policy.History.Mode != governance.HistoryFull || !policy.History.SearchResult) {
						t.Fatalf("web.search history policy = %+v", policy.History)
					}
					if toolName == "web.fetch" && (policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult) {
						t.Fatalf("web.fetch history policy = %+v", policy.History)
					}
				}
				reserved := false
				for _, name := range reg.Names() {
					reserved = reserved || name == toolName
				}
				if !reserved {
					t.Fatalf("%s name was not reserved", toolName)
				}
			}
		})
	}
}

func TestRegisterLimitsWebSearchFailuresAndUnproductiveResults(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range reg.EnabledBuiltinNames() {
		policy, ok := reg.Policy(name)
		if !ok {
			t.Fatalf("missing policy for %s", name)
		}
		wantExecutions := 0
		wantUnproductive := 0
		wantFailures := 0
		if name == "web.search" {
			wantUnproductive = 2
			wantFailures = 2
		} else if name == "web.fetch" {
			wantExecutions = 4
			wantUnproductive = 2
			wantFailures = 2
		} else if name == toolnames.UserMemorySave {
			wantExecutions = 2
		}
		if policy.MaxExecutions != wantExecutions {
			t.Fatalf("%s max executions = %d, want %d", name, policy.MaxExecutions, wantExecutions)
		}
		if policy.MaxUnproductive != wantUnproductive {
			t.Fatalf("%s max unproductive = %d, want %d", name, policy.MaxUnproductive, wantUnproductive)
		}
		if policy.MaxFailures != wantFailures {
			t.Fatalf("%s max failures = %d, want %d", name, policy.MaxFailures, wantFailures)
		}
	}
}

func TestRegisterComfyUIProviderMatrix(t *testing.T) {
	for _, test := range []struct {
		name, url string
		enabled   bool
	}{
		{name: "blank"}, {name: "whitespace", url: "  "}, {name: "configured", url: "http://localhost:8188", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := config.NewLogger(config.LevelError)
			reg := newTestRegistry(t, log)
			cfg := testConfig()
			cfg.ComfyUIURL = test.url
			cfg.ComfyUIGenerationTimeout = 2 * time.Minute
			cfg.ComfyUITextToImageWorkflowPath = filepath.Join("..", "..", "..", "data", "workflows", "comfyui", "text-to-image-basic.json")
			cfg.ComfyUIImageToImageWorkflowPath = filepath.Join("..", "..", "..", "data", "workflows", "comfyui", "image-to-image-basic.json")
			if err := Register(reg, cfg, nil, nil, log); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
				_, visible := visibleTestTool(reg, name)
				if visible != test.enabled || reg.HasHandler(name) != test.enabled {
					t.Fatalf("%s visible=%t handler=%t", name, visible, reg.HasHandler(name))
				}
				reserved := false
				for _, loaded := range reg.Names() {
					reserved = reserved || loaded == name
				}
				if !reserved {
					t.Fatalf("%s is not reserved", name)
				}
				if test.enabled {
					policy, ok := reg.Policy(name)
					if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
						t.Fatalf("%s policy=%+v", name, policy)
					}
				}
			}
		})
	}
}

func TestRegisterComfyUISchemasExposePromptsAndImageSource(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	cfg := testConfig()
	cfg.ComfyUIURL = "http://localhost:8188"
	cfg.ComfyUIGenerationTimeout = 2 * time.Minute
	cfg.ComfyUITextToImageWorkflowPath = filepath.Join("..", "..", "..", "data", "workflows", "comfyui", "text-to-image-basic.json")
	cfg.ComfyUIImageToImageWorkflowPath = filepath.Join("..", "..", "..", "data", "workflows", "comfyui", "image-to-image-basic.json")
	if err := Register(reg, cfg, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
		tool, ok := visibleTestTool(reg, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		schema := tool.Function.Parameters
		wantProperties := 2
		if name == toolnames.ComfyUIImageToImage {
			wantProperties = 3
			if _, ok := schema.Properties["source_image_id"]; !ok {
				t.Fatal("missing image source selector")
			}
		}
		if len(schema.Properties) != wantProperties || len(schema.Required) != 1 || schema.Required[0] != "prompt" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Fatalf("%s schema=%+v", name, schema)
		}
		prompt, ok := schema.Properties["prompt"]
		if !ok {
			t.Fatalf("%s missing prompt", name)
		}
		negative, ok := schema.Properties["negative_prompt"]
		if !ok {
			t.Fatalf("%s missing negative_prompt", name)
		}
		if prompt.MinLength == nil || *prompt.MinLength != 1 || prompt.MaxLength == nil || *prompt.MaxLength != 2000 || negative.MaxLength == nil || *negative.MaxLength != 2000 {
			t.Fatalf("%s prompt bounds are missing: %+v", name, schema.Properties)
		}
	}
}

func TestMemoryArgumentNormalizationPreservesInvalidLimitCorrection(t *testing.T) {
	normalize := normalizeMemorySearchArgs(8)
	invalid, err := governance.Fingerprint(toolnames.GlobalMemorySearch, map[string]interface{}{"query": "test", "limit": "8"}, normalize)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := governance.Fingerprint(toolnames.GlobalMemorySearch, map[string]interface{}{"query": "test", "limit": float64(8)}, normalize)
	if err != nil {
		t.Fatal(err)
	}
	if invalid == valid {
		t.Fatal("invalid limit and corrected valid limit produced the same fingerprint")
	}
}
