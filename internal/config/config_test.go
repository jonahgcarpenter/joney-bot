package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Load mutates the environment through godotenv. Register restoration even for
// initially unset keys, and keep defaults tests away from an operator's .env.
func isolateConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{
		"HOME_ASSISTANT_LISTEN_PORT", "HOME_ASSISTANT_AUTH_TOKEN",
		"BLUEBUBBLES_LISTEN_PORT", "BLUEBUBBLES_URL", "BLUEBUBBLES_PASSWORD",
		"MCP_CONFIG_ENCRYPTION_KEY", "DISCORD_TOKEN",
		"LLM_GATEWAY_URL", "LLM_GATEWAY_MODEL", "LLM_GATEWAY_EMBEDDING_MODEL",
		"LLM_GATEWAY_API_KEY", "LLM_GATEWAY_VIRTUAL_KEY",
		"MODEL_CONTEXT_WINDOW", "MODEL_MAX_OUTPUT_TOKENS",
		"BRAVE_API_KEY", "SEARXNG_URL", "COMFYUI_URL",
		"COMFYUI_TEXT_TO_IMAGE_WORKFLOW", "COMFYUI_IMAGE_TO_IMAGE_WORKFLOW",
		"COMFYUI_GENERATION_TIMEOUT", "WORKER_POOL_SIZE", "LOG_LEVEL",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset test configuration key %s: %v", key, err)
		}
	}
}

func TestEnvHelpersUseFallbacksForMissingEmptyAndInvalidValues(t *testing.T) {
	t.Setenv("OSWALD_TEST_MISSING", "")
	if err := os.Unsetenv("OSWALD_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_TEST_STRING", "")
	t.Setenv("OSWALD_TEST_INT", "not-an-int")

	if got := getEnv("OSWALD_TEST_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("getEnv missing = %q, want fallback", got)
	}
	if got := getEnv("OSWALD_TEST_STRING", "fallback"); got != "" {
		t.Fatalf("getEnv set empty = %q, want empty", got)
	}
	if got := getEnvInt("OSWALD_TEST_INT", 12); got != 12 {
		t.Fatalf("getEnvInt invalid = %d, want 12", got)
	}
}

func TestEnvHelpersParseConfiguredValues(t *testing.T) {
	t.Setenv("OSWALD_TEST_STRING", "value")
	t.Setenv("OSWALD_TEST_INT", "42")

	if got := getEnv("OSWALD_TEST_STRING", "fallback"); got != "value" {
		t.Fatalf("getEnv set = %q, want value", got)
	}
	if got := getEnvInt("OSWALD_TEST_INT", 0); got != 42 {
		t.Fatalf("getEnvInt set = %d, want 42", got)
	}
}

func TestLoadModelBudgetConfig(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("MODEL_CONTEXT_WINDOW", "65536")
	t.Setenv("MODEL_MAX_OUTPUT_TOKENS", "4096")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelContextWindow != 65536 || cfg.ModelMaxOutputTokens != 4096 {
		t.Fatalf("unexpected model budget config: context=%d output=%d", cfg.ModelContextWindow, cfg.ModelMaxOutputTokens)
	}
}

func TestParseLevelAndRequestID(t *testing.T) {
	if got := ParseLevel(" warning "); got != LevelWarn {
		t.Fatalf("ParseLevel warning = %s, want warn", got)
	}
	if got := ParseLevel("unknown"); got != LevelInfo {
		t.Fatalf("ParseLevel unknown = %s, want info", got)
	}

	id := NewRequestID()
	if !strings.HasPrefix(id, "req_") || len(id) != len("req_")+16 {
		t.Fatalf("NewRequestID() = %q, want req_ plus 16 hex chars", id)
	}
}

func TestLoadReadsHomeAssistantConfig(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("HOME_ASSISTANT_AUTH_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HOME_ASSISTANT_LISTEN_PORT", "8124")
	t.Setenv("BLUEBUBBLES_LISTEN_PORT", "8125")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HomeAssistantAuthToken != "0123456789abcdef0123456789abcdef" || cfg.HomeAssistantListenPort != "8124" || cfg.BlueBubblesListenPort != "8125" {
		t.Fatalf("unexpected gateway config: token_set=%t home_assistant_port=%s bluebubbles_port=%s", cfg.HomeAssistantAuthToken != "", cfg.HomeAssistantListenPort, cfg.BlueBubblesListenPort)
	}
}

func TestLoadLeavesOptionalGatewayPortsDisabledByDefault(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("HOME_ASSISTANT_AUTH_TOKEN", "")
	t.Setenv("HOME_ASSISTANT_LISTEN_PORT", "")
	t.Setenv("BLUEBUBBLES_LISTEN_PORT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HomeAssistantAuthToken != "" || cfg.HomeAssistantListenPort != "" || cfg.BlueBubblesListenPort != "" {
		t.Fatalf("unexpected gateway defaults: token_set=%t home_assistant_port=%q bluebubbles_port=%q", cfg.HomeAssistantAuthToken != "", cfg.HomeAssistantListenPort, cfg.BlueBubblesListenPort)
	}
}

func TestLoadOptionalWebSearchProviders(t *testing.T) {
	tests := []struct {
		name        string
		brave       string
		searxng     string
		wantBrave   string
		wantSearxng string
	}{
		{name: "neither"},
		{name: "brave only", brave: "brave-secret", wantBrave: "brave-secret"},
		{name: "searxng only", searxng: "https://search.example", wantSearxng: "https://search.example"},
		{name: "both", brave: "brave-secret", searxng: "https://search.example", wantBrave: "brave-secret", wantSearxng: "https://search.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateConfigEnvironment(t)
			t.Setenv("BRAVE_API_KEY", test.brave)
			t.Setenv("SEARXNG_URL", test.searxng)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BraveAPIKey != test.wantBrave || cfg.SearxngURL != test.wantSearxng {
				t.Fatal("web search configuration did not match the synthetic provider settings")
			}
		})
	}
}

func TestLoadComfyUIDefaultsAndValidation(t *testing.T) {
	isolateConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL != "" || cfg.ComfyUITextToImageWorkflowPath != DefaultComfyUITextToImageWorkflowPath || cfg.ComfyUIImageToImageWorkflowPath != DefaultComfyUIImageToImageWorkflowPath || cfg.ComfyUIGenerationTimeout != 2*time.Minute {
		t.Fatal("ComfyUI configuration did not use the expected defaults")
	}

	for _, invalid := range []string{"localhost:8188", "ftp://example.com", "http:///missing", "http://user@example.com", "http://example.com?x=1", "http://example.com#fragment"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("COMFYUI_URL", invalid)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COMFYUI_URL") {
				t.Fatalf("Load error = %v", err)
			}
		})
	}
}

func TestLoadComfyUIOverrides(t *testing.T) {
	isolateConfigEnvironment(t)
	t.Setenv("COMFYUI_URL", " https://comfy.example/base ")
	t.Setenv("COMFYUI_TEXT_TO_IMAGE_WORKFLOW", "text.json")
	t.Setenv("COMFYUI_IMAGE_TO_IMAGE_WORKFLOW", "image.json")
	t.Setenv("COMFYUI_GENERATION_TIMEOUT", "45s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL != "https://comfy.example/base" || cfg.ComfyUITextToImageWorkflowPath != "text.json" || cfg.ComfyUIImageToImageWorkflowPath != "image.json" || cfg.ComfyUIGenerationTimeout != 45*time.Second {
		t.Fatal("ComfyUI configuration did not match the synthetic overrides")
	}
}

func TestLoadRejectsInvalidComfyUITimeout(t *testing.T) {
	for _, value := range []string{"", "0s", "-1s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			isolateConfigEnvironment(t)
			t.Setenv("COMFYUI_GENERATION_TIMEOUT", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COMFYUI_GENERATION_TIMEOUT") {
				t.Fatalf("Load error = %v", err)
			}
		})
	}
}

func TestLoadEnvironmentIsolationRestoresCallerSettings(t *testing.T) {
	t.Setenv("COMFYUI_GENERATION_TIMEOUT", "invalid-inherited-value")
	t.Setenv("DISCORD_TOKEN", "synthetic-inherited-token")
	t.Run("isolated", func(t *testing.T) {
		isolateConfigEnvironment(t)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DiscordToken != "" || cfg.ComfyUIGenerationTimeout != 2*time.Minute {
			t.Fatal("isolated configuration inherited caller settings")
		}
	})
	if os.Getenv("COMFYUI_GENERATION_TIMEOUT") != "invalid-inherited-value" || os.Getenv("DISCORD_TOKEN") != "synthetic-inherited-token" {
		t.Fatal("configuration fixture did not restore caller settings")
	}
}

func TestLoadDotEnvRespectsExplicitEnvironmentAndEmptyValues(t *testing.T) {
	isolateConfigEnvironment(t)
	const dotenv = "LLM_GATEWAY_MODEL=file-model\nLLM_GATEWAY_API_KEY=synthetic-file-key\nMODEL_CONTEXT_WINDOW=16384\nCOMFYUI_GENERATION_TIMEOUT=30s\n"
	if err := os.WriteFile(".env", []byte(dotenv), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_GATEWAY_MODEL", "environment-model")
	t.Setenv("LLM_GATEWAY_API_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayModel != "environment-model" || cfg.LLMGatewayAPIKey != "" {
		t.Fatal("dotenv overrode an explicitly set environment value")
	}
	if cfg.ModelContextWindow != 16384 || cfg.ComfyUIGenerationTimeout != 30*time.Second {
		t.Fatal("dotenv did not supply unset configuration values")
	}
}

func TestDefaultRetentionPolicy(t *testing.T) {
	want := RetentionPolicy{
		RetiredIndexRetention:    168 * time.Hour,
		SessionInactivity:        24 * time.Hour,
		PendingDeliveryTimeout:   15 * time.Minute,
		SuccessfulJobRetention:   168 * time.Hour,
		DeadJobRetention:         720 * time.Hour,
		AccountChallengeGrace:    24 * time.Hour,
		MaintenanceInterval:      time.Hour,
		DatabaseOptimizeInterval: 24 * time.Hour,
		BatchSize:                100,
	}
	if got := DefaultRetentionPolicy(); got != want {
		t.Fatalf("DefaultRetentionPolicy() = %+v, want %+v", got, want)
	}
}

func TestLoadIgnoresRetiredPolicyEnvironment(t *testing.T) {
	isolateConfigEnvironment(t)
	before, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultRetentionPolicy()
	for _, key := range []string{
		"MEMORY_RETIRED_INDEX_RETENTION", "MEMORY_SESSION_INACTIVITY", "MEMORY_PENDING_DELIVERY_TIMEOUT",
		"MEMORY_SUCCESSFUL_JOB_RETENTION", "MEMORY_DEAD_JOB_RETENTION", "MEMORY_ACCOUNT_CHALLENGE_GRACE",
		"MEMORY_MAINTENANCE_INTERVAL", "MEMORY_DATABASE_OPTIMIZE_INTERVAL", "MEMORY_MAINTENANCE_BATCH_SIZE",
		"MAX_TOOL_CALLS_PER_REQUEST", "MAX_TOOL_ITERATIONS_PER_REQUEST", "MAX_TOOL_FAILURE_RETRIES",
	} {
		t.Setenv(key, "-1")
	}
	after, err := Load()
	if err != nil {
		t.Fatalf("retired policy environment must be ignored: %v", err)
	}
	if *after != *before || DefaultRetentionPolicy() != policy {
		t.Fatal("retired policy environment changed configuration or retention defaults")
	}
}
