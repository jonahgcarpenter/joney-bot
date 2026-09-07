package discord

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// Name returns the human-readable gateway name.
func (dg *Gateway) Name() string {
	return "Discord"
}

// Start initializes the resilient connection loop.
// It blocks forever, automatically reconnecting if the websocket drops.
func (dg *Gateway) Start(b *broker.Broker) error {
	dg.Broker = b
	log := dg.log()
	if dg.replyIndex == nil {
		dg.replyIndex = make(map[string]replyContext)
	}
	dg.setHeartbeatAcked(true)

	for {
		err := dg.connectAndListen()

		if err != nil {
			switch {
			case errors.Is(err, errDiscordReconnectRequested):
				log.Info("gateway.session.resume_requested", "discord requested session resume")
			case errors.Is(err, errDiscordInvalidSession):
				log.Warn("gateway.session.invalid", "discord session invalid, reidentifying")
			case errors.Is(err, errDiscordMissedHeartbeatAck):
				log.Warn("gateway.heartbeat.missed_ack", "discord heartbeat ack missing, reconnecting")
			default:
				log.Warn("gateway.connection.dropped", "discord connection dropped", config.ErrorField(err))
			}
		} else {
			log.Debug("gateway.connection.closed", "discord connection closed normally")
		}

		log.Debug("gateway.reconnect.scheduled", "scheduled discord reconnect", config.F("delay_ms", 5000))
		time.Sleep(5 * time.Second)
	}
}

func (dg *Gateway) runtimeDependencies() gatewayruntime.Dependencies {
	deps := dg.Runtime
	deps.Broker = dg.Broker
	if deps.Access == nil {
		deps.Access = dg.Links
	}
	if deps.Log == nil {
		deps.Log = dg.Log
	}
	return deps
}

// Gateway runs the Discord gateway connection loop.
type Gateway struct {
	Token          string
	BotID          string
	Broker         *broker.Broker
	Links          *accounts.Service
	Runtime        gatewayruntime.Dependencies
	Log            *config.Logger
	APIBaseURL     string
	HTTPClient     *http.Client
	VideoFrames    media.VideoFrameExtractor
	replyMu        sync.RWMutex
	replyIndex     map[string]replyContext
	sessionMu      sync.RWMutex
	sessionID      string
	resumeURL      string
	lastSeq        *int
	hbAcked        bool
	outboundMu     sync.Mutex
	outbound       []*outboundEntry
	outboundActive *outboundEntry
	outboundBytes  int
	outboundClosed bool
	outboundCancel context.CancelFunc
	outboundDone   chan struct{}
	// deliveryContext is set only on a request-local REST client.
	deliveryContext context.Context
}

func (dg *Gateway) log(scoped ...*config.Logger) *config.Logger {
	if len(scoped) > 0 && scoped[0] != nil {
		return scoped[0]
	}
	return dg.Log.Server("gateway.discord", config.F("gateway", "discord"))
}
