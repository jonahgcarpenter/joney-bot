package webfetch

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultOEmbedURL = "https://publish.x.com/oembed"

const fetchTimeout = 15 * time.Second

type fetchHTTPError struct {
	status  int
	message string
}

func (e *fetchHTTPError) Error() string       { return e.message }
func (e *fetchHTTPError) HTTPStatusCode() int { return e.status }

// Fetcher retrieves one public page and returns normalized readable content.
type Fetcher interface {
	Fetch(context.Context, string) (Response, error)
}

// Client retrieves public pages through an SSRF-resistant HTTP transport.
type Client struct {
	httpClient *http.Client
	oEmbedURL  string
}

// NewClient constructs the production direct-fetch client.
func NewClient() *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
	return newClient(net.DefaultResolver, dialer.DialContext, defaultOEmbedURL)
}

func newClient(resolve resolver, dial dialContextFunc, oEmbedURL string) *Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            secureDialContext(resolve, dial),
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
		ResponseHeaderTimeout:  7 * time.Second,
		TLSHandshakeTimeout:    7 * time.Second,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &Client{oEmbedURL: oEmbedURL}
	client.httpClient = &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many redirects")
			}
			validated, err := validateURL(req.URL.String())
			if err != nil {
				return errors.New("redirect target is not eligible for public fetching")
			}
			if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && validated.Scheme != "https" {
				return errors.New("https redirect cannot downgrade to http")
			}
			req.URL = validated
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Del("Referer")
			return nil
		},
	}
	return client
}

// Fetch retrieves a public URL, preferring X's public oEmbed representation for posts.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	parsed, err := validateURL(rawURL)
	if err != nil {
		return Response{}, err
	}
	if canonical, ok := canonicalXPostURL(parsed); ok {
		response, oEmbedErr := c.fetchXPost(ctx, canonical)
		if oEmbedErr == nil {
			return response, nil
		}
		response, err = c.fetchDirect(ctx, parsed)
		if err == nil {
			response.IsDegraded = true
		}
		return response, err
	}
	return c.fetchDirect(ctx, parsed)
}

func (c *Client) fetchDirect(ctx context.Context, target *url.URL) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Response{}, errors.New("could not create fetch request")
	}
	req.Header.Set("Accept", "text/html, application/xhtml+xml, text/plain, application/json, application/*+json")
	req.Header.Set("User-Agent", "oswald-ai/web.fetch")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, errors.New("public page fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Response{}, &fetchHTTPError{status: resp.StatusCode, message: "public page returned an unsuccessful status"}
	}
	if resp.ContentLength > maxBodyBytes {
		return Response{}, errors.New("public page exceeded the size limit")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return Response{}, errors.New("public page exceeded the size limit")
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		contentType = http.DetectContentType(body)
	}
	title, content, normalizedType, err := extractContent(body, contentType)
	if err != nil {
		return Response{}, err
	}
	return Response{
		URL:         resp.Request.URL.String(),
		Title:       title,
		ContentType: normalizedType,
		Source:      "direct",
		Content:     content,
	}, nil
}

func canonicalXPostURL(parsed *url.URL) (string, bool) {
	host := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www."), "mobile.")
	if host != "x.com" && host != "twitter.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] != "status" || parts[2] == "" {
		return "", false
	}
	for _, r := range parts[2] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return "https://x.com/" + url.PathEscape(parts[0]) + "/status/" + parts[2], true
}

func (c *Client) fetchXPost(ctx context.Context, canonical string) (Response, error) {
	endpoint, err := url.Parse(c.oEmbedURL)
	if err != nil {
		return Response{}, errors.New("X post adapter is unavailable")
	}
	query := endpoint.Query()
	query.Set("url", canonical)
	query.Set("omit_script", "true")
	query.Set("dnt", "true")
	query.Set("hide_thread", "false")
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Response{}, errors.New("could not create X post request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "oswald-ai/web.fetch")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, errors.New("X post adapter failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Response{}, &fetchHTTPError{status: resp.StatusCode, message: "X post adapter returned an unsuccessful status"}
	}
	if contentType, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type")); parseErr != nil || contentType != "application/json" {
		return Response{}, errors.New("X post adapter returned an invalid content type")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOEmbedBytes+1))
	if err != nil || len(body) > maxOEmbedBytes {
		return Response{}, errors.New("X post adapter response exceeded the size limit")
	}
	var embed oEmbedResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&embed); err != nil || strings.TrimSpace(embed.HTML) == "" {
		return Response{}, errors.New("X post adapter returned an invalid response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("X post adapter returned an invalid response")
	}
	_, content, err := extractHTML(strings.NewReader(embed.HTML))
	if err != nil || content == "" {
		return Response{}, errors.New("X post adapter returned no readable content")
	}
	title := "X post"
	if author := normalizeVisibleText(embed.AuthorName); author != "" {
		title += " by " + author
	}
	return Response{
		URL:         canonical,
		Title:       truncateRunes(title, maxTitleRunes),
		ContentType: "text/plain",
		Source:      "x_oembed",
		Content:     content,
	}, nil
}

// DecodeToolResponse decodes the bounded web.fetch envelope for streaming consumers.
func DecodeToolResponse(raw string) (Response, error) {
	if len(raw) > maxToolResponseBytes {
		return Response{}, errors.New("decode web fetch tool response: response exceeded size limit")
	}
	var response Response
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&response); err != nil {
		return Response{}, fmt.Errorf("decode web fetch tool response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Response{}, errors.New("decode web fetch tool response: trailing data")
	}
	return response, nil
}
