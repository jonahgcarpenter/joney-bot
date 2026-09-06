package imessage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func (g *Gateway) refreshBlueBubblesCapabilitiesWithRetry(maxAttempts int, delay time.Duration, scoped ...*config.Logger) bool {
	started := time.Now()
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	log := g.log(scoped...)
	var loaded bool
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var available bool
		loaded, available = g.refreshBlueBubblesCapabilities(log)
		if available {
			log.Info("gateway.bluebubbles.capabilities", "resolved BlueBubbles capabilities", config.F("attempt_count", attempt), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("is_private_api_enabled", true), config.F("is_helper_connected", true), config.F("status", "ok"))
			return true
		}
		if attempt < maxAttempts && delay > 0 {
			time.Sleep(delay)
		}
		if loaded {
			log.Debug("gateway.bluebubbles.capabilities_retry", "BlueBubbles private API/helper not ready", config.F("attempt", attempt), config.F("attempt_count", maxAttempts), config.F("status", "degraded"))
		}
	}
	reason := "probe_failed"
	if loaded {
		reason = "private_api_or_helper_unavailable"
	}
	log.Warn("gateway.bluebubbles.capabilities_unavailable", "BlueBubbles private API/helper unavailable after retries", config.F("attempt_count", maxAttempts), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("is_probe_successful", loaded), config.F("reason_code", reason), config.F("status", "degraded"))
	return false
}

func (g *Gateway) refreshBlueBubblesCapabilities(scoped ...*config.Logger) (bool, bool) {
	log := g.log(scoped...)
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/server/info", g.BlueBubblesPassword)
	if err != nil {
		log.Debug("gateway.bluebubbles.capabilities_failed", "failed to build BlueBubbles server info request", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		log.Debug("gateway.bluebubbles.capabilities_failed", "failed to build BlueBubbles server info request", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Debug("gateway.bluebubbles.capabilities_failed", "failed to fetch BlueBubbles server info", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.bluebubbles.capabilities_failed", "BlueBubbles server info failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return false, false
	}

	var result serverInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Debug("gateway.bluebubbles.capabilities_failed", "failed to decode BlueBubbles server info", config.F("status", "degraded"), config.ErrorField(err))
		return false, false
	}

	g.capabilityMu.Lock()
	g.capabilitiesLoaded = true
	g.privateAPIEnabled = result.Data.PrivateAPI
	g.helperConnected = result.Data.HelperConnected
	g.capabilityMu.Unlock()

	log.Debug("gateway.bluebubbles.capabilities_probe", "probed BlueBubbles capabilities", config.F("is_private_api_enabled", result.Data.PrivateAPI), config.F("is_helper_connected", result.Data.HelperConnected), config.F("status", "ok"))
	return true, result.Data.PrivateAPI && result.Data.HelperConnected
}

func (g *Gateway) blueBubblesPrivateAPIAvailable(scoped ...*config.Logger) bool {
	g.capabilityMu.Lock()
	if !g.capabilitiesLoaded {
		g.capabilityMu.Unlock()
		g.refreshBlueBubblesCapabilitiesWithRetry(1, 0, scoped...)
		g.capabilityMu.Lock()
	}
	available := g.privateAPIEnabled && g.helperConnected
	g.capabilityMu.Unlock()
	return available
}

// startTyping enables the typing indicator for the given chat.
func (g *Gateway) startTyping(chatGUID string, scoped ...*config.Logger) error {
	return g.sendTypingRequest(chatGUID, scoped...)
}

// sendTypingRequest sends a BlueBubbles typing request for the given chat.
func (g *Gateway) sendTypingRequest(chatGUID string, scoped ...*config.Logger) error {
	if !g.blueBubblesPrivateAPIAvailable(scoped...) {
		return nil
	}
	endpoint, err := buildBlueBubblesChatActionEndpoint(g.BlueBubblesURL, chatGUID, "typing", g.BlueBubblesPassword)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build BlueBubbles typing request: %w", err)
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send BlueBubbles typing request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log(scoped...).Debug("gateway.typing.failed", "BlueBubbles typing request failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return fmt.Errorf("BlueBubbles typing request failed with status %d", resp.StatusCode)
	}
	return nil
}

// markRead sends a read receipt for the given chat when BlueBubbles supports it.
func (g *Gateway) markRead(chatGUID string, scoped ...*config.Logger) {
	log := g.log(scoped...)
	if !g.blueBubblesPrivateAPIAvailable(log) {
		return
	}
	endpoint, err := buildBlueBubblesChatActionEndpoint(g.BlueBubblesURL, chatGUID, "read", g.BlueBubblesPassword)
	if err != nil {
		log.Debug("gateway.read_receipt.failed", "failed to build BlueBubbles read receipt request", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		log.Debug("gateway.read_receipt.failed", "failed to build BlueBubbles read receipt request", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		log.Debug("gateway.read_receipt.failed", "failed to send BlueBubbles read receipt", config.F("status", "degraded"), config.ErrorField(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Debug("gateway.read_receipt.failed", "BlueBubbles read receipt failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
	}
}

// sendTextReply sends a text reply, retrying with the fallback method if needed.
func (g *Gateway) sendTextReply(chatGUID, text, selectedMessageGUID string, partIndex int, scoped ...*config.Logger) (string, error) {
	log := g.log(scoped...)
	if strings.TrimSpace(selectedMessageGUID) == "" || !g.blueBubblesPrivateAPIAvailable(log) {
		return g.sendText(chatGUID, text, "", 0, "", log)
	}

	messageGUID, err := g.sendText(chatGUID, text, selectedMessageGUID, partIndex, defaultSendMethod, log)
	if err == nil {
		return messageGUID, nil
	}
	log.Warn("gateway.send.retry", "retrying imessage send without private reply fields", config.F("default_method", defaultSendMethod), config.F("status", "retry"), config.ErrorField(err))
	return g.sendText(chatGUID, text, "", 0, "", log)
}

// sendText posts a text message to BlueBubbles and returns the created message GUID.
func (g *Gateway) sendText(chatGUID, text, selectedMessageGUID string, partIndex int, method string, scoped ...*config.Logger) (string, error) {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/text", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload := sendTextRequest{
		ChatGUID:            chatGUID,
		Message:             text,
		Method:              method,
		SelectedMessageGUID: selectedMessageGUID,
		PartIndex:           partIndex,
		TempGUID:            newTempGUID(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles send payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles request: %w", err)
	}
	defer resp.Body.Close()

	var result sendTextResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles send response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.log(scoped...).Debug("gateway.send.provider_failed", "BlueBubbles send failed", config.F("http_status", resp.StatusCode), config.F("has_provider_error", result.Error != nil), config.F("status", "error"))
		return "", fmt.Errorf("BlueBubbles send failed with status %d", resp.StatusCode)
	}
	return result.Data.GUID, nil
}

// buildBlueBubblesEndpoint constructs an authenticated BlueBubbles REST endpoint.
func buildBlueBubblesEndpoint(baseURL, path, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	query := parsed.Query()
	query.Set("password", password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesMessageEndpoint(baseURL, messageGUID, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/message/" + messageGUID
	parsed.RawPath = baseEscapedPath + "/api/v1/message/" + url.PathEscape(messageGUID)
	query := parsed.Query()
	query.Set("password", password)
	query.Set("with", "chats,participants,attachment,handle")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesAttachmentEndpoint(baseURL, attachmentGUID, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/attachment/" + attachmentGUID + "/download"
	parsed.RawPath = baseEscapedPath + "/api/v1/attachment/" + url.PathEscape(attachmentGUID) + "/download"
	query := parsed.Query()
	query.Set("password", password)
	query.Set("original", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildBlueBubblesChatActionEndpoint(baseURL, chatGUID, action, password string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse BlueBubbles URL: %w", err)
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseEscapedPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath + "/api/v1/chat/" + chatGUID + "/" + action
	parsed.RawPath = baseEscapedPath + "/api/v1/chat/" + url.PathEscape(chatGUID) + "/" + url.PathEscape(action)
	query := parsed.Query()
	query.Set("password", password)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// newTempGUID returns a temporary GUID for outbound BlueBubbles send requests.
func newTempGUID() string {
	return fmt.Sprintf("oswald-%d", time.Now().UnixNano())
}

func (g *Gateway) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}
