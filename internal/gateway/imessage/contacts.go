package imessage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func (g *Gateway) lookupContactDisplayName(normalizedSenderID string, scoped ...*config.Logger) (string, error) {
	if normalizedSenderID == "" {
		return "", nil
	}

	if cachedName, ok := g.cachedContactDisplayName(normalizedSenderID); ok {
		return cachedName, nil
	}

	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/contact/query", g.BlueBubblesPassword)
	if err != nil {
		return "", err
	}

	payload, err := json.Marshal(contactQueryRequest{Addresses: []string{normalizedSenderID}})
	if err != nil {
		return "", fmt.Errorf("marshal BlueBubbles contact query: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build BlueBubbles contact query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("send BlueBubbles contact query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		g.log(scoped...).Debug("gateway.contact_lookup.failed", "BlueBubbles contact query failed", config.F("http_status", resp.StatusCode), config.F("response_bytes", len(body)), config.F("status", "degraded"))
		return "", fmt.Errorf("BlueBubbles contact query failed with status %d", resp.StatusCode)
	}

	var result contactQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode BlueBubbles contact query response: %w", err)
	}

	name := chooseContactDisplayName(result.Data)
	g.cacheContactDisplayName(normalizedSenderID, name)
	return name, nil
}

func chooseContactDisplayName(contacts []contactRecord) string {
	for _, contact := range contacts {
		if name := strings.TrimSpace(contact.DisplayName); name != "" {
			return name
		}
		fullName := strings.TrimSpace(strings.TrimSpace(contact.FirstName) + " " + strings.TrimSpace(contact.LastName))
		if fullName != "" {
			return fullName
		}
		if nickname := strings.TrimSpace(contact.Nickname); nickname != "" {
			return nickname
		}
	}
	return ""
}

func (g *Gateway) cachedContactDisplayName(normalizedSenderID string) (string, bool) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	entry, ok := g.contactNames[normalizedSenderID]
	if !ok {
		return "", false
	}
	return entry.DisplayName, true
}

func (g *Gateway) cacheContactDisplayName(normalizedSenderID, displayName string) {
	g.contactMu.Lock()
	defer g.contactMu.Unlock()
	g.pruneContactNamesLocked()
	g.contactNames[normalizedSenderID] = contactNameCacheEntry{
		DisplayName: displayName,
		ExpiresAt:   time.Now().Add(contactCacheTTL),
	}
}

func (g *Gateway) pruneContactNamesLocked() {
	now := time.Now()
	for senderID, entry := range g.contactNames {
		if !entry.ExpiresAt.After(now) {
			delete(g.contactNames, senderID)
		}
	}
}
