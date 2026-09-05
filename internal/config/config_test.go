package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

var retentionEnvKeys = []string{
	"MEMORY_RETIRED_INDEX_RETENTION",
	"MEMORY_SESSION_INACTIVITY",
	"MEMORY_PENDING_DELIVERY_TIMEOUT",
	"MEMORY_SUCCESSFUL_JOB_RETENTION",
	"MEMORY_DEAD_JOB_RETENTION",
	"MEMORY_ACCOUNT_CHALLENGE_GRACE",
	"MEMORY_MAINTENANCE_INTERVAL",
	"MEMORY_DATABASE_OPTIMIZE_INTERVAL",
	"MEMORY_MAINTENANCE_BATCH_SIZE",
}

func TestEnvHelpersUseFallbacksForMissingEmptyAndInvalidValues(t *testing.T) {
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

func TestLoadToolGovernanceLimits(t *testing.T) {
	t.Setenv("MAX_TOOL_CALLS_PER_REQUEST", "15")
	t.Setenv("MAX_TOOL_ITERATIONS_PER_REQUEST", "9")
	t.Setenv("MAX_TOOL_FAILURE_RETRIES", "4")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxToolCallsPerRequest != 15 || cfg.MaxToolIterations != 9 || cfg.MaxToolFailureRetries != 4 {
		t.Fatalf("unexpected tool limits: calls=%d iterations=%d failures=%d", cfg.MaxToolCallsPerRequest, cfg.MaxToolIterations, cfg.MaxToolFailureRetries)
	}
}

func TestLoadRejectsInvalidToolGovernanceLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "calls", key: "MAX_TOOL_CALLS_PER_REQUEST", value: "0"},
		{name: "iterations", key: "MAX_TOOL_ITERATIONS_PER_REQUEST", value: "-1"},
		{name: "failures", key: "MAX_TOOL_FAILURE_RETRIES", value: "-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MAX_TOOL_CALLS_PER_REQUEST", "12")
			t.Setenv("MAX_TOOL_ITERATIONS_PER_REQUEST", "8")
			t.Setenv("MAX_TOOL_FAILURE_RETRIES", "3")
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("Load error = %v, want %s validation", err, test.key)
			}
		})
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
			t.Setenv("BRAVE_API_KEY", test.brave)
			t.Setenv("SEARXNG_URL", test.searxng)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BraveAPIKey != test.wantBrave || cfg.SearxngURL != test.wantSearxng {
				t.Fatalf("web search config = brave:%q searxng:%q", cfg.BraveAPIKey, cfg.SearxngURL)
			}
		})
	}
}

func TestLoadComfyUIDefaultsAndValidation(t *testing.T) {
	for _, key := range []string{"COMFYUI_URL", "COMFYUI_TEXT_TO_IMAGE_WORKFLOW", "COMFYUI_IMAGE_TO_IMAGE_WORKFLOW", "COMFYUI_GENERATION_TIMEOUT"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL != "" || cfg.ComfyUITextToImageWorkflowPath != DefaultComfyUITextToImageWorkflowPath || cfg.ComfyUIImageToImageWorkflowPath != DefaultComfyUIImageToImageWorkflowPath || cfg.ComfyUIGenerationTimeout != 2*time.Minute {
		t.Fatalf("unexpected ComfyUI defaults: %+v", cfg)
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
	t.Setenv("COMFYUI_URL", " https://comfy.example/base ")
	t.Setenv("COMFYUI_TEXT_TO_IMAGE_WORKFLOW", "text.json")
	t.Setenv("COMFYUI_IMAGE_TO_IMAGE_WORKFLOW", "image.json")
	t.Setenv("COMFYUI_GENERATION_TIMEOUT", "45s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComfyUIURL != "https://comfy.example/base" || cfg.ComfyUITextToImageWorkflowPath != "text.json" || cfg.ComfyUIImageToImageWorkflowPath != "image.json" || cfg.ComfyUIGenerationTimeout != 45*time.Second {
		t.Fatalf("unexpected ComfyUI config: %+v", cfg)
	}
}

func TestLoadRejectsInvalidComfyUITimeout(t *testing.T) {
	t.Setenv("COMFYUI_GENERATION_TIMEOUT", "0s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COMFYUI_GENERATION_TIMEOUT") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRetentionPolicyDefaults(t *testing.T) {
	unsetRetentionEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
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
	if cfg.RetentionPolicy != want {
		t.Fatalf("RetentionPolicy = %+v, want %+v", cfg.RetentionPolicy, want)
	}
}

func TestLoadRetentionPolicyOverrides(t *testing.T) {
	unsetRetentionEnv(t)
	overrides := map[string]string{
		"MEMORY_RETIRED_INDEX_RETENTION":    "4h",
		"MEMORY_SESSION_INACTIVITY":         "5h",
		"MEMORY_PENDING_DELIVERY_TIMEOUT":   "6h",
		"MEMORY_SUCCESSFUL_JOB_RETENTION":   "8h",
		"MEMORY_DEAD_JOB_RETENTION":         "9h",
		"MEMORY_ACCOUNT_CHALLENGE_GRACE":    "10h",
		"MEMORY_MAINTENANCE_INTERVAL":       "11h",
		"MEMORY_DATABASE_OPTIMIZE_INTERVAL": "12h",
		"MEMORY_MAINTENANCE_BATCH_SIZE":     "12",
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RetentionPolicy{
		RetiredIndexRetention:    4 * time.Hour,
		SessionInactivity:        5 * time.Hour,
		PendingDeliveryTimeout:   6 * time.Hour,
		SuccessfulJobRetention:   8 * time.Hour,
		DeadJobRetention:         9 * time.Hour,
		AccountChallengeGrace:    10 * time.Hour,
		MaintenanceInterval:      11 * time.Hour,
		DatabaseOptimizeInterval: 12 * time.Hour,
		BatchSize:                12,
	}
	if cfg.RetentionPolicy != want {
		t.Fatalf("RetentionPolicy = %+v, want %+v", cfg.RetentionPolicy, want)
	}
}

func TestLoadRejectsInvalidRetentionValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty duration", key: "MEMORY_RETIRED_INDEX_RETENTION", value: ""},
		{name: "malformed duration", key: "MEMORY_SESSION_INACTIVITY", value: "tomorrow"},
		{name: "zero pending delivery timeout", key: "MEMORY_PENDING_DELIVERY_TIMEOUT", value: "0s"},
		{name: "zero duration", key: "MEMORY_SUCCESSFUL_JOB_RETENTION", value: "0s"},
		{name: "negative duration", key: "MEMORY_MAINTENANCE_INTERVAL", value: "-1h"},
		{name: "empty integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: ""},
		{name: "malformed integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "many"},
		{name: "zero integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "0"},
		{name: "negative integer", key: "MEMORY_MAINTENANCE_BATCH_SIZE", value: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetRetentionEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("Load error = %v, want error naming %s", err, tt.key)
			}
		})
	}
}

func TestLoadRejectsInvalidRetentionRelationships(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantKey   string
	}{
		{
			name: "dead job shorter than successful job",
			overrides: map[string]string{
				"MEMORY_SUCCESSFUL_JOB_RETENTION": "2h",
				"MEMORY_DEAD_JOB_RETENTION":       "1h",
			},
			wantKey: "MEMORY_DEAD_JOB_RETENTION",
		},
		{
			name: "optimize shorter than maintenance",
			overrides: map[string]string{
				"MEMORY_MAINTENANCE_INTERVAL":       "2h",
				"MEMORY_DATABASE_OPTIMIZE_INTERVAL": "1h",
			},
			wantKey: "MEMORY_DATABASE_OPTIMIZE_INTERVAL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetRetentionEnv(t)
			for key, value := range tt.overrides {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.wantKey) {
				t.Fatalf("Load error = %v, want relationship error naming %s", err, tt.wantKey)
			}
		})
	}
}

func unsetRetentionEnv(t *testing.T) {
	t.Helper()
	for _, key := range retentionEnvKeys {
		value, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if exists {
				if err := os.Setenv(key, value); err != nil {
					t.Errorf("restore %s: %v", key, err)
				}
				return
			}
			if err := os.Unsetenv(key); err != nil {
				t.Errorf("unset restored %s: %v", key, err)
			}
		})
	}
}
