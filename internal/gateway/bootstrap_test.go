package gateway

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

const testHomeAssistantToken = "0123456789abcdef0123456789abcdef"

func TestNewServicesFromConfigEnablesConfiguredGateways(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	links := accounts.NewService(dbPath, memorytest.NewStore(t, dbPath, log), nil, log)

	runtimeDeps := gatewayruntime.Dependencies{Log: log}
	services, err := NewServicesFromConfig(&config.Config{HomeAssistantListenPort: "8000", HomeAssistantAuthToken: testHomeAssistantToken}, links, runtimeDeps, log)
	if err != nil {
		t.Fatalf("home assistant services: %v", err)
	}
	if serviceNames(services) != "Home Assistant" {
		t.Fatalf("unexpected home assistant services %q", serviceNames(services))
	}

	services, err = NewServicesFromConfig(&config.Config{HomeAssistantListenPort: "8000", HomeAssistantAuthToken: testHomeAssistantToken, DiscordToken: "token", BlueBubblesListenPort: "8090", BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}, links, runtimeDeps, log)
	if err != nil {
		t.Fatalf("configured services: %v", err)
	}
	if serviceNames(services) != "Home Assistant, Discord, iMessage" {
		t.Fatalf("unexpected configured services %q", serviceNames(services))
	}
}

func TestNewServicesFromConfigSkipsInvalidHomeAssistantConfiguration(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	links := accounts.NewService(dbPath, memorytest.NewStore(t, dbPath, log), nil, log)

	for _, test := range []struct {
		name   string
		config config.Config
	}{
		{name: "both missing", config: config.Config{DiscordToken: "discord"}},
		{name: "missing token", config: config.Config{DiscordToken: "discord", HomeAssistantListenPort: "8000"}},
		{name: "missing port", config: config.Config{DiscordToken: "discord", HomeAssistantAuthToken: testHomeAssistantToken}},
		{name: "short token", config: config.Config{DiscordToken: "discord", HomeAssistantListenPort: "8000", HomeAssistantAuthToken: "short"}},
		{name: "non-numeric port", config: config.Config{DiscordToken: "discord", HomeAssistantListenPort: "invalid", HomeAssistantAuthToken: testHomeAssistantToken}},
		{name: "zero port", config: config.Config{DiscordToken: "discord", HomeAssistantListenPort: "0", HomeAssistantAuthToken: testHomeAssistantToken}},
		{name: "large port", config: config.Config{DiscordToken: "discord", HomeAssistantListenPort: "65536", HomeAssistantAuthToken: testHomeAssistantToken}},
	} {
		t.Run(test.name, func(t *testing.T) {
			services, err := NewServicesFromConfig(&test.config, links, gatewayruntime.Dependencies{Log: log}, log)
			if err != nil || serviceNames(services) != "Discord" {
				t.Fatalf("services=%q err=%v", serviceNames(services), err)
			}
		})
	}
}

func TestNewServicesFromConfigValidatesBlueBubblesConfiguration(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(t.TempDir(), "oswald.db")
	links := accounts.NewService(dbPath, memorytest.NewStore(t, dbPath, log), nil, log)
	runtimeDeps := gatewayruntime.Dependencies{Log: log}

	services, err := NewServicesFromConfig(&config.Config{BlueBubblesListenPort: "8090", BlueBubblesURL: "http://bluebubbles.local", BlueBubblesPassword: "password"}, links, runtimeDeps, log)
	if err != nil || serviceNames(services) != "iMessage" {
		t.Fatalf("valid services=%q err=%v", serviceNames(services), err)
	}

	for _, test := range []struct {
		name   string
		config config.Config
	}{
		{name: "missing port", config: config.Config{DiscordToken: "discord", BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}},
		{name: "missing url", config: config.Config{DiscordToken: "discord", BlueBubblesListenPort: "8090", BlueBubblesPassword: "pw"}},
		{name: "missing password", config: config.Config{DiscordToken: "discord", BlueBubblesListenPort: "8090", BlueBubblesURL: "http://bb"}},
		{name: "invalid port", config: config.Config{DiscordToken: "discord", BlueBubblesListenPort: "invalid", BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}},
		{name: "invalid url", config: config.Config{DiscordToken: "discord", BlueBubblesListenPort: "8090", BlueBubblesURL: "bb.local", BlueBubblesPassword: "pw"}},
		{name: "url credentials", config: config.Config{DiscordToken: "discord", BlueBubblesListenPort: "8090", BlueBubblesURL: "http://user@bb.local", BlueBubblesPassword: "pw"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			services, err := NewServicesFromConfig(&test.config, links, runtimeDeps, log)
			if err != nil || serviceNames(services) != "Discord" {
				t.Fatalf("services=%q err=%v", serviceNames(services), err)
			}
		})
	}
}

func TestNewServicesFromConfigFailsWithoutValidGateway(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	dbPath := filepath.Join(t.TempDir(), "oswald.db")
	links := accounts.NewService(dbPath, memorytest.NewStore(t, dbPath, log), nil, log)
	for _, cfg := range []config.Config{
		{},
		{DiscordToken: "   "},
		{HomeAssistantListenPort: "invalid", HomeAssistantAuthToken: testHomeAssistantToken},
		{BlueBubblesListenPort: "8090", BlueBubblesURL: "invalid", BlueBubblesPassword: "pw"},
	} {
		if services, err := NewServicesFromConfig(&cfg, links, gatewayruntime.Dependencies{Log: log}, log); err == nil || len(services) != 0 {
			t.Fatalf("services=%v err=%v", services, err)
		}
	}
}
