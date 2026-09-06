package comfyui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type fakeGenerator struct {
	workflow map[string]node
	output   string
	input    []byte
	result   []byte
	degraded bool
}

func (f *fakeGenerator) Generate(_ context.Context, workflow map[string]node, output string, input []byte) (GeneratedImage, bool, error) {
	f.workflow, f.output, f.input = workflow, output, append([]byte(nil), input...)
	return GeneratedImage{Filename: "comfyui-generated.png", MIMEType: "image/png", Data: f.result, Size: image.Pt(2, 3)}, f.degraded, nil
}

func TestHandlerUsesEmptyNegativePromptAndReturnsDegradedAttachment(t *testing.T) {
	workflow, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	generator := &fakeGenerator{degraded: true, result: testPNG(t)}
	handler := newHandler(TextToImage, workflow, generator, config.NewLogger(config.LevelError), func(context.Context) []requestctx.InputImage { return nil })
	result, err := handler(authenticatedContext(), map[string]interface{}{"prompt": "a lighthouse"})
	if err != nil {
		t.Fatal(err)
	}
	if generator.output != "9" || generator.workflow["7"].Inputs["text"] != "" {
		t.Fatalf("unexpected generation: output=%s workflow=%+v", generator.output, generator.workflow)
	}
	if !result.IsDegraded || result.ReasonCode != "vram_cleanup_failed" || len(result.Attachments) != 1 || !strings.HasPrefix(result.Attachments[0].Filename, "oswald-text-to-image-") {
		t.Fatalf("result=%+v", result)
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 7 || metadata["status"] != "generated" || metadata["mode"] != "text_to_image" || metadata["attachment_count"] != float64(1) || strings.Contains(result.Content, base64.StdEncoding.EncodeToString(generator.result)) {
		t.Fatalf("unexpected model-visible metadata: %s", result.Content)
	}
}

func TestImageHandlerReencodesFirstRequestImageAsFixedPNG(t *testing.T) {
	workflow, err := LoadWorkflow(workflowPath("image-to-image-basic.json"), ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	input := testPNG(t)
	generator := &fakeGenerator{result: testPNG(t)}
	handler := newHandler(ImageToImage, workflow, generator, config.NewLogger(config.LevelError), func(context.Context) []requestctx.InputImage {
		return []requestctx.InputImage{{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(input)}, {MIMEType: "image/png", Data: "ignored"}}
	})
	if _, err := handler(authenticatedContext(), map[string]interface{}{"prompt": "make it moonlit", "negative_prompt": "daylight"}); err != nil {
		t.Fatal(err)
	}
	if generator.output != "30" || len(generator.input) == 0 || generator.workflow["29"].Inputs["image"] != InputImageReference {
		t.Fatalf("unexpected image generation: output=%s input=%d", generator.output, len(generator.input))
	}
}

func authenticatedContext() context.Context {
	return requestctx.WithPrincipal(context.Background(), identity.Principal{
		CanonicalUserID: "user-1", Gateway: "discord", ExternalID: "external-1", Assurance: identity.AssuranceDiscordGateway,
	})
}
