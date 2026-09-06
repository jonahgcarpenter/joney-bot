package imessage

import (
	"net/http"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

// Name returns the human-readable gateway name.
func (g *Gateway) Name() string {
	return "iMessage"
}

// Start initializes the BlueBubbles webhook listener.
func (g *Gateway) Start(b *broker.Broker) error {
	g.Broker = b
	log := g.log()
	if g.messageIndex == nil {
		g.messageIndex = make(map[string]messageContext)
	}
	if g.contactNames == nil {
		g.contactNames = make(map[string]contactNameCacheEntry)
	}
	g.refreshBlueBubblesCapabilitiesWithRetry(capabilityAttempts, capabilityRetryDelay)

	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, g.handleWebhook)

	log.Info("gateway.listen", "imessage gateway listening", config.F("port", g.Port), config.F("path", webhookPath))
	return http.ListenAndServe(":"+g.Port, mux)
}

func (g *Gateway) runtimeDependencies() gatewayruntime.Dependencies {
	deps := g.Runtime
	deps.Broker = g.Broker
	if deps.Access == nil {
		deps.Access = g.Links
	}
	if deps.Log == nil {
		deps.Log = g.Log
	}
	return deps
}

// Gateway receives BlueBubbles webhooks and sends replies via its REST API.
type Gateway struct {
	Port                string
	BlueBubblesURL      string
	BlueBubblesPassword string
	Links               *accounts.Service
	Runtime             gatewayruntime.Dependencies
	Log                 *config.Logger
	Broker              *broker.Broker
	HTTPClient          *http.Client
	capabilityMu        sync.Mutex
	capabilitiesLoaded  bool
	privateAPIEnabled   bool
	helperConnected     bool
	messageMu           sync.RWMutex
	messageIndex        map[string]messageContext
	contactMu           sync.RWMutex
	contactNames        map[string]contactNameCacheEntry
}

func (g *Gateway) log() *config.Logger {
	return g.Log.Server("gateway.imessage", config.F("gateway", "imessage"))
}

// HandleRuntimeInvalidation purges message and contact context owned by the invalidated tenant.
func (g *Gateway) HandleRuntimeInvalidation(event invalidation.Event) {
	sessions := make(map[string]bool, len(event.SessionIDs))
	for _, sessionID := range event.SessionIDs {
		sessions[sessionID] = true
	}
	senders := make(map[string]bool)
	const prefix = "imessage:"
	for _, external := range event.ExternalIdentities {
		if len(external) > len(prefix) && external[:len(prefix)] == prefix {
			senders[external[len(prefix):]] = true
		}
	}
	g.messageMu.Lock()
	for id, ctx := range g.messageIndex {
		if sessions[ctx.SessionKey] || senders[ctx.SenderID] {
			delete(g.messageIndex, id)
		}
	}
	g.messageMu.Unlock()
	g.contactMu.Lock()
	for senderID := range senders {
		delete(g.contactNames, senderID)
	}
	g.contactMu.Unlock()
}
