package builtin

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/comfyui"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/currenttime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/webfetch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, userMemStore *memory.Store, globalMemStore *global.Store, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	comfyURL := strings.TrimSpace(cfg.ComfyUIURL)
	if comfyURL == "" {
		for _, name := range []string{toolnames.ComfyUIImageToImage, toolnames.ComfyUITextToImage} {
			if err := reg.DisableBuiltin(name); err != nil {
				return fmt.Errorf("failed to disable %s tool: %w", name, err)
			}
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled ComfyUI tools because no server is configured", config.F("tool_name", toolnames.ComfyUITextToImage+","+toolnames.ComfyUIImageToImage), config.F("status", "ok"))
	} else {
		textWorkflow, err := comfyui.LoadWorkflow(cfg.ComfyUITextToImageWorkflowPath, comfyui.TextToImage)
		if err != nil {
			return err
		}
		imageWorkflow, err := comfyui.LoadWorkflow(cfg.ComfyUIImageToImageWorkflowPath, comfyui.ImageToImage)
		if err != nil {
			return err
		}
		client, err := comfyui.NewClient(comfyURL, cfg.ComfyUIGenerationTimeout)
		if err != nil {
			return fmt.Errorf("initialize ComfyUI client: %w", err)
		}
		policy := governance.ToolPolicy{
			BlockDuplicates: true, NormalizeArgs: normalizeComfyArgs,
			History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false},
		}
		if err := reg.RegisterHandler(toolnames.ComfyUITextToImage, policy, registry.Handler(comfyui.NewHandler(comfyui.TextToImage, textWorkflow, client, log))); err != nil {
			return fmt.Errorf("initialize %s tool: %w", toolnames.ComfyUITextToImage, err)
		}
		policy.NormalizeArgs = normalizeComfyImageArgs
		if err := reg.RegisterHandler(toolnames.ComfyUIImageToImage, policy, registry.Handler(comfyui.NewHandler(comfyui.ImageToImage, imageWorkflow, client, log))); err != nil {
			return fmt.Errorf("initialize %s tool: %w", toolnames.ComfyUIImageToImage, err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured ComfyUI image tools", config.F("tool_name", toolnames.ComfyUITextToImage+","+toolnames.ComfyUIImageToImage))
	}
	braveKey := strings.TrimSpace(cfg.BraveAPIKey)
	searxngURL := strings.TrimSpace(cfg.SearxngURL)
	var braveClient websearch.Searcher
	var searxngClient websearch.Searcher
	if braveKey != "" {
		client, err := websearch.NewBraveClient(braveKey, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize Brave web.search client: %w", err)
		}
		braveClient = client
	}
	if searxngURL != "" {
		client, err := websearch.NewSearxngClient(searxngURL, log.Server("tool.web.search"))
		if err != nil {
			return fmt.Errorf("failed to initialize SearXNG web.search client: %w", err)
		}
		searxngClient = client
	}

	var searcher websearch.Searcher
	primary := ""
	fallback := ""
	switch {
	case braveClient != nil && searxngClient != nil:
		searcher = websearch.NewFallbackSearcher(braveClient, searxngClient, log)
		primary, fallback = "brave", "searxng"
	case braveClient != nil:
		searcher = braveClient
		primary = "brave"
	case searxngClient != nil:
		searcher = searxngClient
		primary = "searxng"
	default:
		for _, name := range []string{toolnames.WebFetch, toolnames.WebSearch} {
			if err := reg.DisableBuiltin(name); err != nil {
				return fmt.Errorf("failed to disable %s tool: %w", name, err)
			}
		}
		bootstrapLog.Info("tool.bootstrap.disabled", "disabled web tools because no search provider is configured", config.F("tool_name", toolnames.WebSearch+","+toolnames.WebFetch), config.F("status", "ok"))
	}
	if searcher != nil {
		searchPolicy := toolPolicy(2, normalizeSearchArgs)
		searchPolicy.MaxFailures = 2
		if err := reg.RegisterHandler(toolnames.WebSearch, searchPolicy, registry.Handler(websearch.NewHandler(searcher, log))); err != nil {
			return fmt.Errorf("failed to initialize web.search tool: %w", err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", toolnames.WebSearch), config.F("primary_provider", primary), config.F("fallback_provider", fallback))

		fetchPolicy := governance.ToolPolicy{
			MaxExecutions:   4,
			MaxFailures:     2,
			MaxUnproductive: 2,
			BlockDuplicates: true,
			NormalizeArgs:   normalizeFetchArgs,
			History:         governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false},
		}
		if err := reg.RegisterHandler(toolnames.WebFetch, fetchPolicy, registry.Handler(webfetch.NewHandler(webfetch.NewClient(), log))); err != nil {
			return fmt.Errorf("failed to initialize web.fetch tool: %w", err)
		}
		bootstrapLog.Debug("tool.bootstrap.configured", "configured direct web fetch tool", config.F("tool_name", toolnames.WebFetch))
	}

	if err := reg.RegisterHandler(toolnames.CurrentTime, toolPolicy(0, normalizeTimeArgs), registry.Handler(currenttime.NewHandler(time.Now))); err != nil {
		return fmt.Errorf("failed to initialize time.current tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured current time tool", config.F("tool_name", toolnames.CurrentTime))

	savePolicy := governance.ToolPolicy{
		MaxExecutions: 2, BlockDuplicates: true, NormalizeArgs: normalizeMemorySaveArgs,
		History: governance.HistoryPolicy{Mode: governance.HistoryMetadata, SearchResult: false},
	}
	if err := reg.RegisterHandler(toolnames.UserMemorySave, savePolicy, registry.Handler(usermemory.NewSaveHandler(log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemorySave, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemorySave))

	if err := reg.RegisterHandler(toolnames.UserMemorySearch, toolPolicy(0, normalizeMemorySearchArgs(8)), registry.Handler(usermemory.NewSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemorySearch, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemorySearch), config.F("path", config.DefaultDatabasePath))

	if err := reg.RegisterHandler(toolnames.UserMemoryList, toolPolicy(0, normalizeMemorySearchArgs(25)), registry.Handler(usermemory.NewListHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemoryList, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemoryList), config.F("path", config.DefaultDatabasePath))

	if err := reg.RegisterHandler(toolnames.SessionTranscriptSearch, toolPolicy(0, normalizeMemorySearchArgs(5)), registry.Handler(usermemory.NewTranscriptSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.SessionTranscriptSearch, err)
	}

	if err := reg.RegisterHandler(toolnames.GlobalMemorySearch, toolPolicy(0, normalizeMemorySearchArgs(global.DefaultSearchLimit)), registry.Handler(globalmemory.NewSearchHandler(globalMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.GlobalMemorySearch, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured session transcript tool", config.F("tool_name", toolnames.SessionTranscriptSearch))
	bootstrapLog.Debug("tool.bootstrap.configured", "configured global memory tool", config.F("tool_name", toolnames.GlobalMemorySearch))

	return nil
}

func toolPolicy(maxUnproductive int, normalize governance.ArgumentNormalizer) governance.ToolPolicy {
	return governance.ToolPolicy{
		MaxUnproductive: maxUnproductive,
		BlockDuplicates: true,
		NormalizeArgs:   normalize,
		History:         governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true},
	}
}

func normalizeSearchArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"query": normalizedString(args, "query", true)}
}

func normalizeFetchArgs(args map[string]interface{}) interface{} {
	value, _ := args["url"].(string)
	return map[string]interface{}{"url": webfetch.NormalizeURL(value)}
}

func normalizeComfyArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{
		"prompt":          normalizedString(args, "prompt", false),
		"negative_prompt": normalizedString(args, "negative_prompt", false),
	}
}

func normalizeComfyImageArgs(args map[string]interface{}) interface{} {
	normalized := normalizeComfyArgs(args).(map[string]interface{})
	normalized["create_variant"] = false
	if variant, exists := args["create_variant"]; exists {
		normalized["create_variant"] = variant
	}
	if strength, exists := args["strength"]; exists {
		normalized["strength"] = strength
	}
	if source, exists := args["source_image_id"]; exists {
		// Source IDs are exact-match selectors; preserve invalid values for validation.
		normalized["source_image_id"] = source
	}
	return normalized
}

func normalizeTimeArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"timezone": normalizedString(args, "timezone", false)}
}

func normalizeMemorySearchArgs(defaultLimit int) governance.ArgumentNormalizer {
	return func(args map[string]interface{}) interface{} {
		var limit interface{} = defaultLimit
		if raw, exists := args["limit"]; exists && raw != nil {
			if value, ok := numericInt(raw); ok {
				limit = value
			} else {
				limit = map[string]interface{}{"invalid": raw}
			}
		}
		return map[string]interface{}{
			"query":    normalizedString(args, "query", true),
			"scope":    normalizedString(args, "scope", true),
			"category": normalizedString(args, "category", true),
			"limit":    limit,
		}
	}
}

func normalizeMemorySaveArgs(args map[string]interface{}) interface{} {
	rawMemories, _ := args["memories"].([]interface{})
	memories := make([]map[string]interface{}, 0, len(rawMemories))
	for _, raw := range rawMemories {
		item, _ := raw.(map[string]interface{})
		reinforcesID := item["reinforces_memory_id"]
		if reinforcesID == nil {
			reinforcesID = float64(0)
		}
		memories = append(memories, map[string]interface{}{
			"statement":            normalizedString(item, "statement", false),
			"evidence":             normalizedString(item, "evidence", false),
			"category":             normalizedString(item, "category", true),
			"claim_slot":           normalizedString(item, "claim_slot", true),
			"claim_value":          normalizedString(item, "claim_value", true),
			"supersedes":           normalizedString(item, "supersedes", false),
			"evidence_type":        normalizedString(item, "evidence_type", true),
			"confidence":           item["confidence"],
			"reinforces_memory_id": reinforcesID,
		})
	}
	return map[string]interface{}{"memories": memories}
}

func normalizedString(args map[string]interface{}, key string, lower bool) string {
	value, _ := args[key].(string)
	value = strings.Join(strings.Fields(value), " ")
	if lower {
		value = strings.ToLower(value)
	}
	return value
}

func numericInt(value interface{}) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		if !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number) && float64(int(number)) == number {
			return int(number), true
		}
	case float32:
		value := float64(number)
		if !math.IsNaN(value) && !math.IsInf(value, 0) && number == float32(int(number)) {
			return int(number), true
		}
	}
	return 0, false
}
