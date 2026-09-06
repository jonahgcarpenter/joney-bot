package homeassistant

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/invalidation"
)

const protocolVersion = 1

// Gateway serves the authenticated Home Assistant conversation protocol.
type Gateway struct {
	Port          string
	Links         *accounts.Service
	Runtime       gatewayruntime.Dependencies
	Log           *config.Logger
	tokenHash     [sha256.Size]byte
	connectionsMu sync.Mutex
	connections   map[string]map[*trackedConnection]struct{}
}

type conversationRequest struct {
	Type           string `json:"type"`
	RequestID      string `json:"request_id"`
	UserID         string `json:"user_id"`
	DisplayName    string `json:"display_name,omitempty"`
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
}

type protocolMessage struct {
	Type            string                   `json:"type"`
	ProtocolVersion int                      `json:"protocol_version,omitempty"`
	RequestID       string                   `json:"request_id,omitempty"`
	Text            string                   `json:"text,omitempty"`
	Tool            *agent.ToolStreamPayload `json:"tool,omitempty"`
	Response        string                   `json:"response,omitempty"`
	Model           string                   `json:"model,omitempty"`
	Metrics         *agent.ModelMetrics      `json:"metrics,omitempty"`
	Code            string                   `json:"code,omitempty"`
	Message         string                   `json:"message,omitempty"`
}

type trackedConnection struct {
	conn    *gorilla.Conn
	writeMu sync.Mutex
}

func (c *trackedConnection) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := c.conn.WriteJSON(value)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *trackedConnection) closeWithReason(_ string) {
	// Closing the underlying connection interrupts an in-progress blocked write;
	// waiting on writeMu here would delay authorization invalidation.
	_ = c.conn.Close()
}

func (g *Gateway) authenticate(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return false
	}
	presented := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(presented[:], g.tokenHash[:]) == 1
}

// HandleRuntimeInvalidation closes active conversations for removed or disconnected HA users.
func (g *Gateway) HandleRuntimeInvalidation(event invalidation.Event) {
	if !event.CloseConnections {
		return
	}
	for _, external := range event.ExternalIdentities {
		userID, ok := strings.CutPrefix(external, "homeassistant:")
		if !ok || userID == "" {
			continue
		}
		g.connectionsMu.Lock()
		connections := g.connections[userID]
		delete(g.connections, userID)
		g.connectionsMu.Unlock()
		for connection := range connections {
			connection.closeWithReason("authorization revoked")
		}
	}
}

func (g *Gateway) track(userID string, connection *trackedConnection) {
	g.connectionsMu.Lock()
	defer g.connectionsMu.Unlock()
	if g.connections == nil {
		g.connections = make(map[string]map[*trackedConnection]struct{})
	}
	if g.connections[userID] == nil {
		g.connections[userID] = make(map[*trackedConnection]struct{})
	}
	g.connections[userID][connection] = struct{}{}
}

func (g *Gateway) untrack(userID string, connection *trackedConnection) {
	g.connectionsMu.Lock()
	defer g.connectionsMu.Unlock()
	delete(g.connections[userID], connection)
	if len(g.connections[userID]) == 0 {
		delete(g.connections, userID)
	}
}

var upgrader = gorilla.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024, CheckOrigin: func(r *http.Request) bool { return len(r.Header.Values("Origin")) == 0 }}
