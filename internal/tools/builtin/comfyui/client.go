package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

const maxControlResponseBytes = 1 << 20

type comfyHTTPError struct {
	status  int
	message string
}

func (e *comfyHTTPError) Error() string       { return e.message }
func (e *comfyHTTPError) HTTPStatusCode() int { return e.status }

// Client serializes ComfyUI generations, including best-effort VRAM cleanup.
type Client struct {
	base         *url.URL
	httpClient   *http.Client
	timeout      time.Duration
	pollInterval time.Duration
	permit       chan struct{}
}

// NewClient constructs a ComfyUI client for an already validated base URL.
func NewClient(rawURL string, timeout time.Duration) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("ComfyUI URL must be HTTP(S) with a host and without userinfo, query, or fragment")
	}
	if timeout <= 0 {
		return nil, errors.New("ComfyUI generation timeout must be positive")
	}
	return &Client{
		base: base, httpClient: &http.Client{}, timeout: timeout,
		pollInterval: 250 * time.Millisecond, permit: make(chan struct{}, 1),
	}, nil
}

// Generate uploads an optional PNG, submits a workflow, polls its history, and downloads its first output.
// cleanupFailed is meaningful when a valid image is returned.
func (c *Client) Generate(ctx context.Context, workflow map[string]node, outputNode string, inputPNG []byte) (image GeneratedImage, cleanupFailed bool, err error) {
	stageLog := generationStageLogger(ctx)
	phase, started := "permit", time.Now()
	defer func() {
		if phase != "" {
			stageLog(phase, started, err)
		}
	}()
	select {
	case c.permit <- struct{}{}:
	case <-ctx.Done():
		return GeneratedImage{}, false, ctx.Err()
	}
	stageLog(phase, started, nil)
	phase, started = "upload", time.Now()
	defer func() {
		stageLog(phase, started, err)
		phase = ""
		cleanupStarted := time.Now()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		cleanupErr := c.cleanup(cleanupCtx)
		stageLog("cleanup", cleanupStarted, cleanupErr)
		if cleanupErr != nil {
			cleanupFailed = true
		}
		<-c.permit
	}()

	generationCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if inputPNG != nil {
		if err := c.upload(generationCtx, inputPNG); err != nil {
			return GeneratedImage{}, false, err
		}
		stageLog(phase, started, nil)
	}
	phase, started = "submit", time.Now()
	promptID, err := c.submit(generationCtx, workflow)
	if err != nil {
		return GeneratedImage{}, false, err
	}
	stageLog(phase, started, nil)
	phase, started = "poll", time.Now()
	descriptor, err := c.poll(generationCtx, promptID, outputNode)
	if err != nil {
		return GeneratedImage{}, false, err
	}
	stageLog(phase, started, nil)
	phase, started = "download", time.Now()
	generated, err := c.download(generationCtx, descriptor)
	if err != nil {
		return GeneratedImage{}, false, err
	}
	return generated, false, nil
}

func (c *Client) endpoint(suffix string) string {
	endpoint := *c.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + suffix
	endpoint.RawPath = ""
	return endpoint.String()
}

func (c *Client) upload(ctx context.Context, pngData []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", InputFilename)
	if err != nil {
		return errors.New("build ComfyUI upload")
	}
	if _, err := part.Write(pngData); err != nil {
		return errors.New("build ComfyUI upload")
	}
	_ = writer.WriteField("type", "input")
	_ = writer.WriteField("subfolder", InputSubfolder)
	_ = writer.WriteField("overwrite", "true")
	if err := writer.Close(); err != nil {
		return errors.New("build ComfyUI upload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/upload/image"), &body)
	if err != nil {
		return errors.New("build ComfyUI upload request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	var response struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
		Type      string `json:"type"`
	}
	if err := c.doJSON(req, &response); err != nil {
		return fmt.Errorf("ComfyUI upload failed: %w", err)
	}
	if response.Name != InputFilename || response.Subfolder != InputSubfolder || response.Type != "input" {
		return errors.New("ComfyUI upload returned an unexpected image reference")
	}
	return nil
}

func (c *Client) submit(ctx context.Context, workflow map[string]node) (string, error) {
	body, err := json.Marshal(map[string]interface{}{"prompt": workflow, "client_id": "oswald-ai"})
	if err != nil {
		return "", errors.New("encode ComfyUI workflow")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/prompt"), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("build ComfyUI prompt request")
	}
	req.Header.Set("Content-Type", "application/json")
	var response struct {
		PromptID string      `json:"prompt_id"`
		Error    interface{} `json:"error"`
	}
	if err := c.doJSON(req, &response); err != nil {
		return "", fmt.Errorf("ComfyUI prompt submission failed: %w", err)
	}
	if response.Error != nil || strings.TrimSpace(response.PromptID) == "" {
		return "", errors.New("ComfyUI rejected the workflow")
	}
	if strings.ContainsAny(response.PromptID, `/\\`) {
		return "", errors.New("ComfyUI returned an invalid prompt identifier")
	}
	return response.PromptID, nil
}

func (c *Client) poll(ctx context.Context, promptID, outputNode string) (outputDescriptor, error) {
	escapedID := url.PathEscape(promptID)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/history/"+escapedID), nil)
		if err != nil {
			return outputDescriptor{}, errors.New("build ComfyUI history request")
		}
		var history map[string]struct {
			Outputs map[string]struct {
				Images []outputDescriptor `json:"images"`
			} `json:"outputs"`
			Status struct {
				Completed bool   `json:"completed"`
				Status    string `json:"status_str"`
			} `json:"status"`
		}
		if err := c.doJSON(req, &history); err != nil {
			return outputDescriptor{}, fmt.Errorf("ComfyUI history polling failed: %w", err)
		}
		if record, exists := history[promptID]; exists {
			if strings.EqualFold(record.Status.Status, "error") {
				return outputDescriptor{}, errors.New("ComfyUI generation failed")
			}
			if output := record.Outputs[outputNode]; len(output.Images) > 0 {
				if err := validateDescriptor(output.Images[0]); err != nil {
					return outputDescriptor{}, err
				}
				return output.Images[0], nil
			}
			if record.Status.Completed {
				return outputDescriptor{}, errors.New("ComfyUI completed without the expected output")
			}
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return outputDescriptor{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateDescriptor(descriptor outputDescriptor) error {
	if descriptor.Filename == "" || path.Base(descriptor.Filename) != descriptor.Filename || strings.ContainsAny(descriptor.Filename, "\\/") {
		return errors.New("ComfyUI returned an unsafe output filename")
	}
	cleanSubfolder := path.Clean(descriptor.Subfolder)
	hasTraversal := false
	for _, segment := range strings.Split(descriptor.Subfolder, "/") {
		hasTraversal = hasTraversal || segment == ".."
	}
	if descriptor.Subfolder != "" && (cleanSubfolder == "." || cleanSubfolder == ".." || strings.HasPrefix(cleanSubfolder, "../") || strings.HasPrefix(descriptor.Subfolder, "/") || strings.Contains(descriptor.Subfolder, "\\") || hasTraversal) {
		return errors.New("ComfyUI returned an unsafe output subfolder")
	}
	if descriptor.Type != "output" && descriptor.Type != "temp" {
		return errors.New("ComfyUI returned an unsafe output type")
	}
	return nil
}

func (c *Client) download(ctx context.Context, descriptor outputDescriptor) (GeneratedImage, error) {
	endpoint, _ := url.Parse(c.endpoint("/view"))
	query := endpoint.Query()
	query.Set("filename", descriptor.Filename)
	query.Set("subfolder", descriptor.Subfolder)
	query.Set("type", descriptor.Type)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return GeneratedImage{}, errors.New("build ComfyUI image request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return GeneratedImage{}, errors.New("download ComfyUI image")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return GeneratedImage{}, &comfyHTTPError{status: resp.StatusCode, message: "ComfyUI image download returned an unsuccessful status"}
	}
	if resp.ContentLength > media.MaxOutputAttachmentBytes {
		return GeneratedImage{}, errors.New("ComfyUI image exceeds the size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxOutputAttachmentBytes+1))
	if err != nil || len(data) > media.MaxOutputAttachmentBytes {
		return GeneratedImage{}, errors.New("ComfyUI image exceeds the size limit")
	}
	detected := http.DetectContentType(data)
	declared, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if declared != "" && declared != detected {
		return GeneratedImage{}, errors.New("ComfyUI image MIME type does not match its content")
	}
	extension := ""
	switch detected {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		return GeneratedImage{}, errors.New("ComfyUI returned an unsupported image type")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 {
		return GeneratedImage{}, errors.New("ComfyUI returned an invalid image")
	}
	return GeneratedImage{Filename: "comfyui-generated" + extension, MIMEType: detected, Data: data, Size: image.Pt(config.Width, config.Height)}, nil
}

func (c *Client) cleanup(ctx context.Context) error {
	body := []byte(`{"unload_models":true,"free_memory":true}`)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/free"), bytes.NewReader(body))
		if err != nil {
			return errors.New("build ComfyUI cleanup request")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt == 0 {
				continue
			}
			return errors.New("ComfyUI cleanup transport failed")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return nil
		}
		if resp.StatusCode >= 500 && attempt == 0 {
			continue
		}
		return &comfyHTTPError{status: resp.StatusCode, message: "ComfyUI cleanup returned an unsuccessful status"}
	}
	return errors.New("ComfyUI cleanup failed")
}

func (c *Client) doJSON(req *http.Request, destination interface{}) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &comfyHTTPError{status: resp.StatusCode, message: "unsuccessful status"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxControlResponseBytes+1))
	if err != nil || len(body) > maxControlResponseBytes {
		return errors.New("JSON response exceeded the size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid JSON response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON response")
	}
	return nil
}
