package gateway

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/homeassistant"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/imessage"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links *accounts.Service, runtimeDeps gatewayruntime.Dependencies, log *config.Logger) ([]Service, error) {
	gatewayLog := log.Server("gateway.bootstrap")
	services := make([]Service, 0, 3)
	homeAssistantTokenSet := strings.TrimSpace(cfg.HomeAssistantAuthToken) != ""
	homeAssistantPortSet := strings.TrimSpace(cfg.HomeAssistantListenPort) != ""
	if homeAssistantTokenSet && homeAssistantPortSet {
		homeAssistantGateway, err := homeassistant.New(cfg.HomeAssistantListenPort, cfg.HomeAssistantAuthToken, links, runtimeDeps, log)
		if err != nil {
			gatewayLog.Warn("gateway.homeassistant.config_invalid", "home assistant gateway configuration is invalid; gateway disabled", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			services = append(services, homeAssistantGateway)
			if runtimeDeps.RuntimeInvalidationBus != nil {
				runtimeDeps.RuntimeInvalidationBus.Subscribe(homeAssistantGateway.HandleRuntimeInvalidation)
			}
		}
	} else {
		gatewayLog.Debug("gateway.homeassistant.disabled", "home assistant gateway is disabled", config.F("is_token_set", homeAssistantTokenSet), config.F("is_port_set", homeAssistantPortSet))
	}

	discordToken := strings.TrimSpace(cfg.DiscordToken)
	if discordToken != "" {
		discordGateway := &discord.Gateway{
			Token:   discordToken,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		}
		services = append(services, discordGateway)
		if runtimeDeps.RuntimeInvalidationBus != nil {
			runtimeDeps.RuntimeInvalidationBus.Subscribe(discordGateway.HandleRuntimeInvalidation)
		}
	} else {
		gatewayLog.Debug("gateway.discord.disabled", "discord gateway is disabled", config.F("is_token_set", false))
	}

	blueBubblesPortSet := strings.TrimSpace(cfg.BlueBubblesListenPort) != ""
	blueBubblesURLSet := strings.TrimSpace(cfg.BlueBubblesURL) != ""
	blueBubblesPasswordSet := strings.TrimSpace(cfg.BlueBubblesPassword) != ""
	if blueBubblesPortSet && blueBubblesURLSet && blueBubblesPasswordSet {
		port, baseURL, err := validateBlueBubblesConfig(cfg.BlueBubblesListenPort, cfg.BlueBubblesURL)
		if err != nil {
			gatewayLog.Warn("gateway.imessage.config_invalid", "imessage gateway configuration is invalid; gateway disabled", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			iMessageGateway := &imessage.Gateway{
				Port:                port,
				BlueBubblesURL:      baseURL,
				BlueBubblesPassword: cfg.BlueBubblesPassword,
				Links:               links,
				Runtime:             runtimeDeps,
				Log:                 log,
			}
			services = append(services, iMessageGateway)
			if runtimeDeps.RuntimeInvalidationBus != nil {
				runtimeDeps.RuntimeInvalidationBus.Subscribe(iMessageGateway.HandleRuntimeInvalidation)
			}
		}
	} else {
		gatewayLog.Debug("gateway.imessage.disabled", "imessage gateway is disabled", config.F("is_port_set", blueBubblesPortSet), config.F("is_url_set", blueBubblesURLSet), config.F("is_password_set", blueBubblesPasswordSet))
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no gateways are configured correctly")
	}

	gatewayLog.Info("gateway.bootstrap.enabled", "resolved enabled gateways",
		config.F("gateway_count", len(services)),
		config.F("gateways", serviceNames(services)),
	)

	return services, nil
}

func validateBlueBubblesConfig(portValue, urlValue string) (string, string, error) {
	portNumber, err := strconv.Atoi(strings.TrimSpace(portValue))
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("BLUEBUBBLES_LISTEN_PORT must be an integer from 1 through 65535")
	}
	baseURL := strings.TrimSpace(urlValue)
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("BLUEBUBBLES_URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	return strconv.Itoa(portNumber), strings.TrimRight(baseURL, "/"), nil
}

func serviceNames(services []Service) string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name())
	}
	return strings.Join(names, ", ")
}
