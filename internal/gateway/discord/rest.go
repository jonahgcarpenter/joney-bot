package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func (dg *Gateway) fetchMessage(channelID, messageID, requestID string) (messageResponse, bool) {
	log := dg.log()
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" {
		return messageResponse{}, false
	}

	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to build discord reply lookup request", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	req.Header.Set("Authorization", "Bot "+dg.Token)

	resp, err := dg.httpClient(10 * time.Second).Do(req)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to fetch discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to read discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Debug("gateway.reply_lookup.failed", "discord reply lookup failed", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("http_status", resp.StatusCode), config.F("response_bytes", len(respBody)), config.F("status", "degraded"))
		return messageResponse{}, false
	}

	var result messageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		log.Debug("gateway.reply_lookup.failed", "failed to decode discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("status", "degraded"), config.ErrorField(err))
		return messageResponse{}, false
	}
	log.Debug("gateway.reply_lookup.fetched", "fetched discord reply target", config.F("request_id", requestID), config.F("chat_id", channelID), config.F("message_id", messageID), config.F("attachment_count", len(result.Attachments)), config.F("status", "ok"))
	return result, true
}

// sendTyping posts a typing indicator to Discord.
func (dg *Gateway) sendTyping(channelID string, scoped ...*config.Logger) error {
	url := fmt.Sprintf("%s/channels/%s/typing", dg.apiBaseURL(), channelID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+dg.Token)

	resp, err := dg.httpClient(5 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		dg.log(scoped...).Debug("gateway.typing.failed", "discord typing request failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// sendMessage posts a message to a Discord channel and returns the created
// Discord message ID when available.
func (dg *Gateway) sendMessage(channelID, content, replyToID string, scoped ...*config.Logger) (string, error) {
	url := fmt.Sprintf("%s/channels/%s/messages", dg.apiBaseURL(), channelID)

	payload := map[string]interface{}{
		"content": content,
	}

	if replyToID != "" {
		payload["message_reference"] = map[string]string{
			"message_id": replyToID,
		}
	}

	created, err := dg.doMessageJSON(http.MethodPost, url, payload)
	if err != nil {
		dg.log(scoped...).Debug("gateway.send.failed", "discord send request failed", config.F("status", "error"), config.ErrorField(err))
		return "", err
	}
	return created.ID, nil
}

// editMessage replaces the content of a message previously sent by Oswald.
func (dg *Gateway) editMessage(channelID, messageID, content string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	_, err := dg.doMessageJSON(http.MethodPatch, url, map[string]string{"content": content})
	return err
}

func (dg *Gateway) deleteMessage(channelID, messageID string) error {
	url := fmt.Sprintf("%s/channels/%s/messages/%s", dg.apiBaseURL(), channelID, messageID)
	_, err := dg.doMessageJSON(http.MethodDelete, url, nil)
	return err
}

func (dg *Gateway) doMessageJSON(method, url string, payload interface{}) (createMessageResponse, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return createMessageResponse{}, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		var requestBody io.Reader
		if body != nil {
			requestBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(dg.restContext(), method, url, requestBody)
		if err != nil {
			return createMessageResponse{}, err
		}
		req.Header.Set("Authorization", "Bot "+dg.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := dg.httpClient(10 * time.Second).Do(req)
		if err != nil {
			return createMessageResponse{}, err
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := discordRetryAfter(resp, respBody)
			if readErr == nil && dg.deliveryContext == nil && attempt == 0 && retryAfter > 0 && retryAfter <= 5*time.Second {
				time.Sleep(retryAfter)
				continue
			}
			return createMessageResponse{}, discordRateLimitError{retryAfter: retryAfter}
		}
		if readErr != nil {
			return createMessageResponse{}, readErr
		}

		if dg.deliveryContext == nil && resp.StatusCode >= 500 && attempt == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return createMessageResponse{}, discordHTTPError(resp.StatusCode)
		}

		var result createMessageResponse
		if len(respBody) > 0 {
			if err := json.Unmarshal(respBody, &result); err != nil {
				return createMessageResponse{}, err
			}
		}
		return result, nil
	}
	return createMessageResponse{}, fmt.Errorf("discord %s request exhausted retries", method)
}

func (dg *Gateway) restContext() context.Context {
	if dg.deliveryContext != nil {
		return dg.deliveryContext
	}
	return context.Background()
}

func discordRetryAfter(resp *http.Response, body []byte) time.Duration {
	if value := strings.TrimSpace(resp.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
			// No delivery entry can outlive five minutes; cap before conversion to avoid overflow.
			return time.Duration(min(seconds, 300) * float64(time.Second))
		}
	}
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.RetryAfter > 0 {
		return time.Duration(min(payload.RetryAfter, 300) * float64(time.Second))
	}
	return 0
}

func (dg *Gateway) apiBaseURL() string {
	if dg.APIBaseURL != "" {
		return dg.APIBaseURL
	}
	return apiBaseURL
}

func (dg *Gateway) httpClient(timeout time.Duration) *http.Client {
	if dg.HTTPClient != nil {
		return dg.HTTPClient
	}
	return &http.Client{Timeout: timeout}
}
