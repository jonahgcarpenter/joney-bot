// Package homeassistant implements the Home Assistant conversation gateway.
package homeassistant

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

var protocolIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// New creates a Home Assistant gateway with a deployment-scoped service token.
func New(port, token string, links *accounts.Service, runtime gatewayruntime.Dependencies, log *config.Logger) (*Gateway, error) {
	token = strings.TrimSpace(token)
	if len(token) < 32 {
		return nil, fmt.Errorf("HOME_ASSISTANT_AUTH_TOKEN must contain at least 32 characters")
	}
	port = strings.TrimSpace(port)
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("HOME_ASSISTANT_LISTEN_PORT must be an integer from 1 through 65535")
	}
	if links == nil || log == nil {
		return nil, fmt.Errorf("home assistant gateway dependencies are incomplete")
	}
	gateway := &Gateway{Port: strconv.Itoa(portNumber), Links: links, Runtime: runtime, Log: log}
	gateway.tokenHash = sha256Token(token)
	return gateway, nil
}

func sha256Token(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// Name returns the human-readable gateway name.
func (g *Gateway) Name() string { return "Home Assistant" }

// Start serves the Home Assistant WebSocket endpoint.
func (g *Gateway) Start(b *broker.Broker) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/homeassistant/ws", func(w http.ResponseWriter, r *http.Request) { g.handleConnection(w, r, b) })
	g.log().Info("gateway.listen", "home assistant gateway listening", config.F("port", g.Port), config.F("path", "/homeassistant/ws"))
	return http.ListenAndServe(":"+g.Port, mux)
}

func (g *Gateway) handleConnection(w http.ResponseWriter, r *http.Request, b *broker.Broker) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.authenticate(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		g.log().Warn("gateway.authentication.failed", "home assistant authentication failed", config.F("status", "rejected"))
		return
	}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		g.log().Warn("gateway.connection.upgrade_failed", "home assistant websocket upgrade failed", config.ErrorField(err))
		return
	}
	defer connection.Close()
	connection.SetReadLimit(128 << 10)
	tracked := &trackedConnection{conn: connection}
	if err := tracked.writeJSON(protocolMessage{Type: "ready", ProtocolVersion: protocolVersion}); err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	if messageType != gorilla.TextMessage {
		_ = tracked.writeJSON(protocolMessage{Type: "error", Code: "invalid_request", Message: "Only JSON text requests are supported."})
		return
	}
	request, err := decodeRequest(payload)
	if err != nil {
		_ = tracked.writeJSON(protocolMessage{Type: "error", Code: "invalid_request", Message: "The request was invalid."})
		return
	}
	userID, err := accounts.NormalizeIdentifier("homeassistant", request.UserID)
	if err != nil {
		_ = tracked.writeJSON(protocolMessage{Type: "error", RequestID: request.RequestID, Code: "user_required", Message: "An authenticated Home Assistant user is required."})
		return
	}
	canonicalUserID, err := g.Links.EnsureAccount("homeassistant", userID, strings.TrimSpace(request.DisplayName))
	if err != nil {
		_ = tracked.writeJSON(protocolMessage{Type: "error", RequestID: request.RequestID, Code: "service_unavailable", Message: "Oswald could not resolve the Home Assistant user."})
		return
	}
	g.track(userID, tracked)
	defer g.untrack(userID, tracked)
	currentOwner, ownerExists, err := g.Links.ResolveAccount("homeassistant", userID)
	if err != nil || !ownerExists || currentOwner != canonicalUserID {
		_ = tracked.writeJSON(protocolMessage{Type: "error", RequestID: request.RequestID, Code: "service_unavailable", Message: "The Home Assistant user is no longer available."})
		return
	}

	internalRequestID := config.NewRequestID()
	sessionKey := "homeassistant:" + userID + ":" + request.ConversationID
	firstChunk := true
	stream := func(chunk agent.StreamChunk) {
		if chunk.Type == agent.ChunkStatus {
			return
		}
		if firstChunk {
			g.log().Debug("gateway.stream.started", "started home assistant stream", config.F("request_id", internalRequestID), config.F("stream_type", string(chunk.Type)))
			firstChunk = false
		}
		_ = tracked.writeJSON(protocolMessage{Type: string(chunk.Type), RequestID: request.RequestID, Text: chunk.Text, Tool: chunk.Tool})
	}
	principal := identity.Principal{CanonicalUserID: canonicalUserID, Gateway: "homeassistant", ExternalID: userID, Assurance: identity.AssuranceHomeAssistantToken}
	gatewayruntime.Execute(gatewayruntime.Request{
		RequestID: internalRequestID, ChatID: sessionKey, Principal: principal,
		DisplayName: strings.TrimSpace(request.DisplayName), SessionKey: sessionKey,
		IsDirect: true, IsMention: true, Text: request.Text, StreamFunc: stream,
	}, g.runtimeDependencies(b), &runtimeResponder{connection: tracked, requestID: request.RequestID})
	_ = connection.SetWriteDeadline(time.Now().Add(time.Second))
	_ = connection.WriteControl(gorilla.CloseMessage, gorilla.FormatCloseMessage(gorilla.CloseNormalClosure, "complete"), time.Now().Add(time.Second))
}

func decodeRequest(payload []byte) (conversationRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request conversationRequest
	if err := decoder.Decode(&request); err != nil {
		return conversationRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return conversationRequest{}, fmt.Errorf("request must contain one JSON object")
	}
	request.Type = strings.TrimSpace(request.Type)
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.UserID = strings.TrimSpace(request.UserID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	if request.Type != "conversation" || !protocolIDPattern.MatchString(request.RequestID) || !protocolIDPattern.MatchString(request.UserID) || !validConversationID(request.ConversationID) || strings.TrimSpace(request.Text) == "" {
		return conversationRequest{}, fmt.Errorf("invalid conversation request")
	}
	if len(request.Text) > 100000 || len(request.DisplayName) > 512 {
		return conversationRequest{}, fmt.Errorf("conversation request exceeds limits")
	}
	return request, nil
}

func validConversationID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (g *Gateway) runtimeDependencies(b *broker.Broker) gatewayruntime.Dependencies {
	dependencies := g.Runtime
	dependencies.Broker = b
	if dependencies.Access == nil {
		dependencies.Access = g.Links
	}
	if dependencies.Log == nil {
		dependencies.Log = g.Log
	}
	return dependencies
}

func (g *Gateway) log() *config.Logger {
	return g.Log.Server("gateway.homeassistant", config.F("gateway", "homeassistant"))
}
