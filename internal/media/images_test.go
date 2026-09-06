package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestNormalizeInputImageDoesNotResizeSmallJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 640, 480)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "small.jpg")
	if err != nil {
		t.Fatalf("normalize small jpeg: %v", err)
	}

	if result.WasResized {
		t.Fatal("expected small image not to resize")
	}
	if result.OriginalWidth != 640 || result.OriginalHeight != 480 || result.Width != 640 || result.Height != 480 {
		t.Fatalf("unexpected dimensions: original=%dx%d normalized=%dx%d", result.OriginalWidth, result.OriginalHeight, result.Width, result.Height)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", result.Image.MimeType)
	}
	if result.NormalizedBytes == 0 || result.Base64Chars != len(result.Image.Data) {
		t.Fatalf("unexpected encoded sizes: bytes=%d base64=%d data=%d", result.NormalizedBytes, result.Base64Chars, len(result.Image.Data))
	}
}

func TestNormalizeInputImageResizesLargeLandscapeJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 4032, 3024)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "large.jpg")
	if err != nil {
		t.Fatalf("normalize large jpeg: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected large image to resize")
	}
	if result.OriginalWidth != 4032 || result.OriginalHeight != 3024 {
		t.Fatalf("unexpected original dimensions: %dx%d", result.OriginalWidth, result.OriginalHeight)
	}
	if result.Width > MaxNormalizedImageLongEdge || result.Height > MaxNormalizedImageLongEdge {
		t.Fatalf("normalized dimensions exceed cap: %dx%d", result.Width, result.Height)
	}
	if result.NormalizedBytes > MaxNormalizedImageBytes {
		t.Fatalf("normalized bytes = %d, want <= %d", result.NormalizedBytes, MaxNormalizedImageBytes)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", result.Image.MimeType)
	}
}

func TestNormalizeInputImageResizesLargePortraitJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 3024, 4032)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "portrait.jpg")
	if err != nil {
		t.Fatalf("normalize portrait jpeg: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected portrait image to resize")
	}
	if result.Width > MaxNormalizedImageLongEdge || result.Height > MaxNormalizedImageLongEdge {
		t.Fatalf("normalized dimensions exceed cap: %dx%d", result.Width, result.Height)
	}
	if result.NormalizedBytes > MaxNormalizedImageBytes {
		t.Fatalf("normalized bytes = %d, want <= %d", result.NormalizedBytes, MaxNormalizedImageBytes)
	}
}

func TestNormalizeInputImagePreservesTransparentPNG(t *testing.T) {
	raw := encodeTestPNG(t, 3000, 2000, true)

	result, err := NormalizeInputImageFromBytes(nil, "image/png", raw, "transparent.png")
	if err != nil {
		t.Fatalf("normalize transparent png: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected transparent png to resize")
	}
	if !result.PreservedAlpha {
		t.Fatal("expected alpha to be preserved")
	}
	if result.Image.MimeType != "image/png" {
		t.Fatalf("expected image/png, got %q", result.Image.MimeType)
	}
	if result.Width > MaxNormalizedImageLongEdge || result.Height > MaxNormalizedImageLongEdge {
		t.Fatalf("normalized dimensions exceed cap: %dx%d", result.Width, result.Height)
	}
	if result.NormalizedBytes > MaxNormalizedImageBytes {
		t.Fatalf("normalized bytes = %d, want <= %d", result.NormalizedBytes, MaxNormalizedImageBytes)
	}
	if _, err := base64.StdEncoding.DecodeString(result.Image.Data); err != nil {
		t.Fatalf("normalized payload is not valid base64: %v", err)
	}
}

func TestNormalizeInputImageDownscalesToOutputByteCap(t *testing.T) {
	raw := encodeNoisyTestJPEG(t, 2200, 1600)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "noisy.jpg")
	if err != nil {
		t.Fatalf("normalize noisy jpeg: %v", err)
	}

	if result.NormalizedBytes > MaxNormalizedImageBytes {
		t.Fatalf("normalized bytes = %d, want <= %d", result.NormalizedBytes, MaxNormalizedImageBytes)
	}
	if result.Width >= MaxNormalizedImageLongEdge {
		t.Fatalf("expected byte cap to force extra downscale below long edge cap, got %dx%d", result.Width, result.Height)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", result.Image.MimeType)
	}
}

func TestNormalizeInputImageKeepsSingleFrameGIF(t *testing.T) {
	raw := encodeTestGIF(t, []*image.Paletted{solidGIFFrame(image.Rect(0, 0, 8, 6), color.RGBA{R: 255, A: 255})}, nil, nil, 8, 6)

	result, err := NormalizeInputImageFromBytes(nil, "image/gif", raw, "still.gif")
	if err != nil {
		t.Fatalf("normalize single-frame GIF: %v", err)
	}
	if result.DecodedFormat != "gif" || result.Width != 8 || result.Height != 6 {
		t.Fatalf("unexpected GIF result: %+v", result)
	}
	if result.Image.IsGIFContactSheet {
		t.Fatal("single-frame GIF was marked as a contact sheet")
	}
}

func TestNormalizeInputImageBuildsTimelineSampledGIFContactSheet(t *testing.T) {
	colors := []color.RGBA{
		{R: 255, A: 255},
		{G: 255, A: 255},
		{B: 255, A: 255},
		{R: 255, G: 255, A: 255},
		{R: 255, B: 255, A: 255},
	}
	frames := make([]*image.Paletted, len(colors))
	for index, frameColor := range colors {
		frames[index] = solidGIFFrame(image.Rect(0, 0, 6, 4), frameColor)
	}
	raw := encodeTestGIF(t, frames, []int{1, 1, 1, 1, 1}, nil, 6, 4)

	result, err := NormalizeInputImageFromBytes(nil, "image/gif", raw, "animated.gif")
	if err != nil {
		t.Fatalf("normalize animated GIF: %v", err)
	}
	if result.Width != 12 || result.Height != 8 {
		t.Fatalf("contact sheet dimensions = %dx%d, want 12x8", result.Width, result.Height)
	}
	if result.OriginalWidth != 6 || result.OriginalHeight != 4 {
		t.Fatalf("original dimensions = %dx%d, want 6x4", result.OriginalWidth, result.OriginalHeight)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("opaque contact sheet MIME = %q, want image/jpeg", result.Image.MimeType)
	}
	if !result.Image.IsGIFContactSheet {
		t.Fatal("animated GIF was not marked as a contact sheet")
	}
	decoded := decodeInputImage(t, result.Image)
	for index, want := range []color.RGBA{colors[0], colors[1], colors[3], colors[4]} {
		x := index%2*6 + 3
		y := index/2*4 + 2
		assertColorNear(t, decoded.At(x, y), want)
	}
}

func TestNormalizeInputImageCompositesGIFDisposalBackground(t *testing.T) {
	palette := color.Palette{color.Transparent, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}, color.RGBA{G: 255, A: 255}}
	base := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	fillPaletted(base, 1)
	blue := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	fillPaletted(blue, 2)
	green := image.NewPaletted(image.Rect(2, 2, 4, 4), palette)
	fillPaletted(green, 3)
	raw := encodeTestGIF(t, []*image.Paletted{base, blue, green}, nil, []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalNone}, 4, 4)

	result, err := NormalizeInputImageFromBytes(nil, "image/gif", raw, "background.gif")
	if err != nil {
		t.Fatalf("normalize disposal-background GIF: %v", err)
	}
	if result.Image.MimeType != "image/png" {
		t.Fatalf("transparent contact sheet MIME = %q, want image/png", result.Image.MimeType)
	}
	decoded := decodeInputImage(t, result.Image)
	_, _, _, alpha := decoded.At(0, 4).RGBA()
	if alpha != 0 {
		t.Fatalf("disposed area alpha = %d, want transparent", alpha)
	}
	assertColorNear(t, decoded.At(3, 7), color.RGBA{G: 255, A: 255})
}

func TestNormalizeInputImageLimitsGIFContactSheet(t *testing.T) {
	frames := make([]*image.Paletted, 4)
	for index := range frames {
		frames[index] = solidGIFFrame(image.Rect(0, 0, 1300, 10), color.RGBA{R: uint8(index * 60), G: 200, B: 100, A: 255})
	}
	raw := encodeTestGIF(t, frames, nil, nil, 1300, 10)

	result, err := NormalizeInputImageFromBytes(nil, "image/gif", raw, "wide.gif")
	if err != nil {
		t.Fatalf("normalize wide animated GIF: %v", err)
	}
	if !result.WasResized || result.Width > MaxNormalizedImageLongEdge || result.Height > MaxNormalizedImageLongEdge {
		t.Fatalf("contact sheet was not limited: %+v", result)
	}
	if result.NormalizedBytes > MaxNormalizedImageBytes {
		t.Fatalf("normalized bytes = %d, want <= %d", result.NormalizedBytes, MaxNormalizedImageBytes)
	}
}

func TestNormalizeInputImageCompositesGIFDisposalPrevious(t *testing.T) {
	palette := color.Palette{color.Transparent, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}, color.RGBA{G: 255, A: 255}}
	base := image.NewPaletted(image.Rect(0, 0, 4, 4), palette)
	fillPaletted(base, 1)
	blue := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	fillPaletted(blue, 2)
	green := image.NewPaletted(image.Rect(2, 2, 4, 4), palette)
	fillPaletted(green, 3)
	raw := encodeTestGIF(t, []*image.Paletted{base, blue, green}, nil, []byte{gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone}, 4, 4)

	result, err := NormalizeInputImageFromBytes(nil, "image/gif", raw, "previous.gif")
	if err != nil {
		t.Fatalf("normalize disposal-previous GIF: %v", err)
	}
	decoded := decodeInputImage(t, result.Image)
	assertColorNear(t, decoded.At(0, 4), color.RGBA{R: 255, A: 255})
	assertColorNear(t, decoded.At(3, 7), color.RGBA{G: 255, A: 255})
}

func TestResizeInputImagesForAttemptScalesNormalizedImages(t *testing.T) {
	jpegInput, err := NormalizeInputImageFromBytes(nil, "image/jpeg", encodeTestJPEG(t, 800, 600), "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	jpegInput.Image.IsGIFContactSheet = true
	pngInput, err := NormalizeInputImageFromBytes(nil, "image/png", encodeTestPNG(t, 400, 200, true), "alpha.png")
	if err != nil {
		t.Fatal(err)
	}
	resized, err := ResizeInputImagesForAttempt([]llm.InputImage{jpegInput.Image, pngInput.Image}, 1, 0.75, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []image.Point{{X: 600, Y: 450}, {X: 300, Y: 150}} {
		data, err := base64.StdEncoding.DecodeString(resized[i].Data)
		if err != nil {
			t.Fatal(err)
		}
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if got := decoded.Bounds().Size(); got != want {
			t.Fatalf("image %d dimensions = %v, want %v", i, got, want)
		}
	}
	if resized[0].MimeType != "image/jpeg" || resized[1].MimeType != "image/png" || resized[0].Source != "photo.jpg" || resized[1].Source != "alpha.png" {
		t.Fatalf("image metadata was not preserved: %+v", resized)
	}
	if !resized[0].IsGIFContactSheet {
		t.Fatal("GIF contact-sheet metadata was not preserved")
	}
}

func TestFFmpegVideoFrameExtractorBuildsContactSheet(t *testing.T) {
	tempDir := t.TempDir()
	framePath := filepath.Join(tempDir, "frame.png")
	if err := os.WriteFile(framePath, encodeTestPNG(t, 8, 8, false), 0o600); err != nil {
		t.Fatal(err)
	}
	ffprobePath := filepath.Join(tempDir, "ffprobe")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\nprintf '3.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpegPath := filepath.Join(tempDir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nfor output do :; done\ndir=${output%/*}\ncp \"$FRAME_FIXTURE\" \"$dir/frame-1.png\"\ncp \"$FRAME_FIXTURE\" \"$dir/frame-2.png\"\ncp \"$FRAME_FIXTURE\" \"$dir/frame-3.png\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FRAME_FIXTURE", framePath)

	extractor := FFmpegVideoFrameExtractor{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath}
	input, err := extractor.Extract(context.Background(), []byte("video"), "clip.mp4")
	if err != nil {
		t.Fatalf("extract video contact sheet: %v", err)
	}
	decoded := decodeInputImage(t, input)
	if got := decoded.Bounds().Size(); got != (image.Point{X: 16, Y: 16}) {
		t.Fatalf("contact sheet dimensions = %v, want 16x16", got)
	}
	if input.Source != "clip.mp4" {
		t.Fatalf("source = %q, want clip.mp4", input.Source)
	}
	if !input.IsGIFContactSheet {
		t.Fatal("extracted video was not marked as a GIF contact sheet")
	}
}

func TestFFmpegVideoFrameExtractorRejectsEmptySuccessfulOutput(t *testing.T) {
	tempDir := t.TempDir()
	ffprobePath := filepath.Join(tempDir, "ffprobe")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\nprintf '3.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpegPath := filepath.Join(tempDir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	extractor := FFmpegVideoFrameExtractor{FFmpegPath: ffmpegPath, FFprobePath: ffprobePath}
	if _, err := extractor.Extract(context.Background(), []byte("video"), "clip.mp4"); err == nil || !strings.Contains(err.Error(), "produced no video frames") {
		t.Fatalf("error = %v, want no-frames error", err)
	}
}

func encodeTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fillTestImage(img, false)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodeTestPNG(t *testing.T, width, height int, transparent bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fillTestImage(img, transparent)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func encodeNoisyTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var state uint32 = 1
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			state = state*1664525 + 1013904223
			r := uint8(state >> 24)
			state = state*1664525 + 1013904223
			g := uint8(state >> 24)
			state = state*1664525 + 1013904223
			b := uint8(state >> 24)
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode noisy test jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodeTestGIF(t *testing.T, frames []*image.Paletted, delays []int, disposal []byte, width, height int) []byte {
	t.Helper()
	if delays == nil {
		delays = make([]int, len(frames))
	}
	var buf bytes.Buffer
	err := gif.EncodeAll(&buf, &gif.GIF{
		Image:    frames,
		Delay:    delays,
		Disposal: disposal,
		Config: image.Config{
			ColorModel: frames[0].Palette,
			Width:      width,
			Height:     height,
		},
	})
	if err != nil {
		t.Fatalf("encode test GIF: %v", err)
	}
	return buf.Bytes()
}

func solidGIFFrame(bounds image.Rectangle, frameColor color.RGBA) *image.Paletted {
	frame := image.NewPaletted(bounds, color.Palette{frameColor})
	fillPaletted(frame, 0)
	return frame
}

func fillPaletted(frame *image.Paletted, paletteIndex uint8) {
	for index := range frame.Pix {
		frame.Pix[index] = paletteIndex
	}
}

func decodeInputImage(t *testing.T, input llm.InputImage) image.Image {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		t.Fatalf("decode input image base64: %v", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode input image: %v", err)
	}
	return decoded
}

func assertColorNear(t *testing.T, got color.Color, want color.RGBA) {
	t.Helper()
	r, g, b, a := got.RGBA()
	const tolerance = 6000
	for name, values := range map[string][2]uint32{
		"red": {r, uint32(want.R) * 257}, "green": {g, uint32(want.G) * 257},
		"blue": {b, uint32(want.B) * 257}, "alpha": {a, uint32(want.A) * 257},
	} {
		delta := int64(values[0]) - int64(values[1])
		if delta < 0 {
			delta = -delta
		}
		if delta > tolerance {
			t.Fatalf("%s channel = %d, want %d (color %#v)", name, values[0], values[1], got)
		}
	}
}

func fillTestImage(img *image.RGBA, transparent bool) {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			alpha := uint8(255)
			if transparent && (x+y)%17 == 0 {
				alpha = 96
			}
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: alpha})
		}
	}
}
