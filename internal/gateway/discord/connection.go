package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gorilla "github.com/gorilla/websocket"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

var (
	errDiscordReconnectRequested = errors.New("discord requested reconnect")
	errDiscordInvalidSession     = errors.New("discord invalid session")
	errDiscordMissedHeartbeatAck = errors.New("discord missed heartbeat ack")
)

// connectAndListen manages a single Discord gateway session.
func (dg *Gateway) connectAndListen() error {
	resumeURL, shouldResume := dg.resumeGatewayURL()
	conn, _, err := gorilla.DefaultDialer.Dial(resolveGatewayURL(resumeURL), nil)
	if err != nil {
		return fmt.Errorf("Failed to dial Discord Gateway: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()

	var helloPayload Payload
	if err := conn.ReadJSON(&helloPayload); err != nil || helloPayload.Op != 10 {
		return fmt.Errorf("Expected HELLO payload: %v", err)
	}

	var hello HelloEvent
	json.Unmarshal(helloPayload.D, &hello) // nolint: errcheck

	dg.setHeartbeatAcked(true)
	hbErrCh := make(chan error, 1)
	go dg.heartbeatLoop(conn, hello.HeartbeatInterval*time.Millisecond, done, hbErrCh)

	if shouldResume {
		if err := dg.resume(conn); err != nil {
			return fmt.Errorf("Failed to resume: %w", err)
		}
		dg.log().Debug("gateway.session.resume_attempt", "attempted discord session resume")
	} else {
		if err := dg.identify(conn); err != nil {
			return fmt.Errorf("Failed to identify: %w", err)
		}
	}

	err = dg.listenLoop(conn)
	select {
	case hbErr := <-hbErrCh:
		if hbErr != nil {
			return hbErr
		}
	default:
	}

	return err
}

// identify authenticates the bot with its token and intents.
func (dg *Gateway) identify(conn *gorilla.Conn) error {
	identifyData := map[string]interface{}{
		"token":   dg.Token,
		"intents": intents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "oswald-ai",
			"device":  "oswald-ai",
		},
	}

	identifyPayload := Payload{
		Op: 2,
		D:  marshalJSON(identifyData),
	}

	if err := conn.WriteJSON(identifyPayload); err != nil {
		return fmt.Errorf("Failed to send IDENTIFY: %w", err)
	}
	return nil
}

// resume attempts to resume a prior Discord gateway session.
func (dg *Gateway) resume(conn *gorilla.Conn) error {
	sessionID, seq, ok := dg.resumeState()
	if !ok {
		return errors.New("resume state unavailable")
	}

	resumeData := map[string]interface{}{
		"token":      dg.Token,
		"session_id": sessionID,
		"seq":        seq,
	}

	resumePayload := Payload{
		Op: 6,
		D:  marshalJSON(resumeData),
	}

	if err := conn.WriteJSON(resumePayload); err != nil {
		return fmt.Errorf("Failed to send RESUME: %w", err)
	}

	return nil
}

// heartbeatLoop sends heartbeat packets to Discord at the specified interval.
func (dg *Gateway) heartbeatLoop(conn *gorilla.Conn, interval time.Duration, done <-chan struct{}, errCh chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			select {
			case errCh <- nil:
			default:
			}
			return
		case <-ticker.C:
		}

		if !dg.heartbeatAcked() {
			select {
			case errCh <- errDiscordMissedHeartbeatAck:
			default:
			}
			_ = conn.Close()
			return
		}

		dg.setHeartbeatAcked(false)

		hb := Payload{Op: 1, D: dg.heartbeatPayload()}
		if err := conn.WriteJSON(hb); err != nil {
			select {
			case <-done:
				return
			default:
			}

			if isClosedConnError(err) {
				return
			}

			select {
			case errCh <- fmt.Errorf("discord heartbeat failed: %w", err):
			default:
			}
			_ = conn.Close()
			return
		}
	}
}

// listenLoop reads events from the Discord gateway and dispatches them.
func (dg *Gateway) listenLoop(conn *gorilla.Conn) error {
	for {
		var p Payload
		if err := conn.ReadJSON(&p); err != nil {
			return fmt.Errorf("Discord read error: %w", err)
		}

		if p.S != nil {
			dg.setLastSequence(*p.S)
		}

		switch p.Op {
		case 0:
			if p.T != nil {
				switch *p.T {
				case "READY":
					var ready ReadyEvent
					if err := json.Unmarshal(p.D, &ready); err == nil {
						dg.BotID = ready.User.ID
						dg.setReadySession(ready.SessionID, ready.ResumeGatewayURL)
						dg.setHeartbeatAcked(true)
						dg.log().Info("gateway.session.ready", "discord gateway ready", config.F("bot_id", dg.BotID), config.F("bot_username", ready.User.Username))
					}
				case "RESUMED":
					dg.setHeartbeatAcked(true)
					dg.log().Debug("gateway.session.resumed", "discord session resumed")
				case "MESSAGE_CREATE":
					var msg MessageCreate
					if err := json.Unmarshal(p.D, &msg); err == nil {
						go dg.handleMessage(msg)
					}
				}
			}
		case 7:
			return errDiscordReconnectRequested
		case 9:
			dg.clearResumeState()
			return errDiscordInvalidSession
		case 11:
			dg.setHeartbeatAcked(true)
		}
	}
}

func (dg *Gateway) setReadySession(sessionID, resumeURL string) {
	dg.sessionMu.Lock()
	dg.sessionID = sessionID
	dg.resumeURL = resumeURL
	dg.sessionMu.Unlock()
}

func (dg *Gateway) clearResumeState() {
	dg.sessionMu.Lock()
	dg.sessionID = ""
	dg.resumeURL = ""
	dg.lastSeq = nil
	dg.hbAcked = true
	dg.sessionMu.Unlock()
}

func (dg *Gateway) setLastSequence(seq int) {
	dg.sessionMu.Lock()
	seqCopy := seq
	dg.lastSeq = &seqCopy
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatPayload() json.RawMessage {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.lastSeq == nil {
		return json.RawMessage("null")
	}
	return marshalJSON(*dg.lastSeq)
}

func (dg *Gateway) setHeartbeatAcked(acked bool) {
	dg.sessionMu.Lock()
	dg.hbAcked = acked
	dg.sessionMu.Unlock()
}

func (dg *Gateway) heartbeatAcked() bool {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.hbAcked
}

func (dg *Gateway) resumeState() (string, int, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	if dg.sessionID == "" || dg.lastSeq == nil {
		return "", 0, false
	}
	return dg.sessionID, *dg.lastSeq, true
}

func (dg *Gateway) resumeGatewayURL() (string, bool) {
	dg.sessionMu.RLock()
	defer dg.sessionMu.RUnlock()
	return dg.resumeURL, dg.sessionID != "" && dg.lastSeq != nil
}

func resolveGatewayURL(resumeURL string) string {
	if resumeURL == "" {
		return gatewayURL
	}
	if strings.HasPrefix(resumeURL, "ws://") || strings.HasPrefix(resumeURL, "wss://") {
		return resumeURL
	}
	return fmt.Sprintf("wss://%s/?v=10&encoding=json", resumeURL)
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "closed network connection")
}

// marshalJSON converts any value to a json.RawMessage, ignoring marshal errors.
func marshalJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
