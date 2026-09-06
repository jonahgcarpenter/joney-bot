package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	HomeAssistantListenPort         string        // Optional HTTP port for the Home Assistant gateway
	HomeAssistantAuthToken          string        // Optional shared bearer token; gateway disabled if empty
	BlueBubblesListenPort           string        // Optional HTTP port for the BlueBubbles webhook listener
	BlueBubblesURL                  string        // BlueBubbles HTTP(S) base URL
	BlueBubblesPassword             string        // BlueBubbles server password/token for REST API auth
	MCPConfigEncryptionKey          string        // Key used to encrypt MCP server URLs and headers at rest
	LLMGatewayURL                   string        // LLM gateway API base URL (default: "http://localhost:8080")
	LLMGatewayModel                 string        // LLM gateway model name; required, startup fails if empty
	LLMGatewayEmbeddingModel        string        // Optional LLM gateway embedding model used for semantic durable-memory retrieval
	LLMGatewayAPIKey                string        // Optional bearer token for LLM gateway requests
	LLMGatewayVirtualKey            string        // Optional gateway routing key for LLM gateway requests
	ModelContextWindow              int           // Optional model context window for prompt budgeting; non-positive uses the package fallback
	ModelMaxOutputTokens            int           // Foreground output capacity reserve and private extraction/compaction max_tokens; non-positive uses the package fallback
	DiscordToken                    string        // Optional Discord bot token
	BraveAPIKey                     string        // Optional Brave Search API subscription token
	SearxngURL                      string        // Optional SearXNG base URL for web search
	ComfyUIURL                      string        // Optional ComfyUI HTTP(S) base URL; image tools are disabled if empty
	ComfyUITextToImageWorkflowPath  string        // ComfyUI text-to-image API workflow path
	ComfyUIImageToImageWorkflowPath string        // ComfyUI image-to-image API workflow path
	ComfyUIGenerationTimeout        time.Duration // Maximum duration of one ComfyUI generation
	WorkerPoolSize                  int           // Number of concurrent broker workers (default: 1)
	LogLevel                        Level         // Logging verbosity (default: LevelInfo)
}

// RetentionPolicy controls content expiry and periodic memory maintenance.
type RetentionPolicy struct {
	RetiredIndexRetention    time.Duration
	SessionInactivity        time.Duration
	PendingDeliveryTimeout   time.Duration
	SuccessfulJobRetention   time.Duration
	DeadJobRetention         time.Duration
	AccountChallengeGrace    time.Duration
	MaintenanceInterval      time.Duration
	DatabaseOptimizeInterval time.Duration
	BatchSize                int
}

// DefaultRetentionPolicy returns the code-owned memory retention and maintenance policy.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
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
}

const (
	DefaultSoulPath                        = "data/memory/soul/soul.md"
	DefaultToolsConfigDir                  = "data/tools"
	DefaultDatabasePath                    = "data/database/oswald.db"
	DefaultComfyUITextToImageWorkflowPath  = "data/workflows/comfyui/text-to-image-basic.json"
	DefaultComfyUIImageToImageWorkflowPath = "data/workflows/comfyui/image-to-image-basic.json"
)

// Load reads configuration from environment variables, with .env file support.
// Missing variables use defaults; invalid security-sensitive values return an error.
func Load() (*Config, error) {
	// Silently ignore missing .env — production environments use real env vars
	godotenv.Load() // nolint: errcheck
	comfyTimeout, err := getEnvPositiveDuration("COMFYUI_GENERATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		HomeAssistantListenPort:         getEnv("HOME_ASSISTANT_LISTEN_PORT", ""),
		HomeAssistantAuthToken:          getEnv("HOME_ASSISTANT_AUTH_TOKEN", ""),
		BlueBubblesListenPort:           getEnv("BLUEBUBBLES_LISTEN_PORT", ""),
		BlueBubblesURL:                  getEnv("BLUEBUBBLES_URL", ""),
		BlueBubblesPassword:             getEnv("BLUEBUBBLES_PASSWORD", ""),
		MCPConfigEncryptionKey:          getEnv("MCP_CONFIG_ENCRYPTION_KEY", ""),
		LLMGatewayURL:                   getEnv("LLM_GATEWAY_URL", "http://localhost:8080"),
		LLMGatewayModel:                 getEnv("LLM_GATEWAY_MODEL", ""),
		LLMGatewayEmbeddingModel:        getEnv("LLM_GATEWAY_EMBEDDING_MODEL", ""),
		LLMGatewayAPIKey:                getEnv("LLM_GATEWAY_API_KEY", ""),
		LLMGatewayVirtualKey:            getEnv("LLM_GATEWAY_VIRTUAL_KEY", ""),
		ModelContextWindow:              getEnvInt("MODEL_CONTEXT_WINDOW", 0),
		ModelMaxOutputTokens:            getEnvInt("MODEL_MAX_OUTPUT_TOKENS", 0),
		DiscordToken:                    getEnv("DISCORD_TOKEN", ""),
		BraveAPIKey:                     getEnv("BRAVE_API_KEY", ""),
		SearxngURL:                      getEnv("SEARXNG_URL", ""),
		ComfyUIURL:                      strings.TrimSpace(getEnv("COMFYUI_URL", "")),
		ComfyUITextToImageWorkflowPath:  getEnv("COMFYUI_TEXT_TO_IMAGE_WORKFLOW", DefaultComfyUITextToImageWorkflowPath),
		ComfyUIImageToImageWorkflowPath: getEnv("COMFYUI_IMAGE_TO_IMAGE_WORKFLOW", DefaultComfyUIImageToImageWorkflowPath),
		ComfyUIGenerationTimeout:        comfyTimeout,
		WorkerPoolSize:                  getEnvInt("WORKER_POOL_SIZE", 1),
		LogLevel:                        ParseLevel(getEnv("LOG_LEVEL", "info")),
	}
	if cfg.ComfyUIURL != "" {
		parsed, err := url.Parse(cfg.ComfyUIURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("COMFYUI_URL must be an HTTP(S) URL with a host and without userinfo, query, or fragment")
		}
	}
	return cfg, nil
}

// getEnv retrieves an environment variable with a fallback to the default value
// if the variable is not set.
func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// getEnvInt retrieves an environment variable as an integer with a fallback default.
// Returns the default if the variable is missing or cannot be parsed as an integer.
func getEnvInt(key string, defaultValue int) int {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return n
}

func getEnvPositiveDuration(key string, defaultValue time.Duration) (time.Duration, error) {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return d, nil
}
