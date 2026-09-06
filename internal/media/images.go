package media

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	imagedraw "image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"sort"
	"strings"

	_ "github.com/jdeng/goheif"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// MaxImagesPerRequest limits multimodal fan-in to keep requests bounded.
	MaxImagesPerRequest = 4
	// MaxImageBytes limits each decoded image payload accepted from a gateway.
	MaxImageBytes = 10 << 20
	// MaxNormalizedImageLongEdge limits the pixels sent to vision models while preserving useful detail.
	MaxNormalizedImageLongEdge = 2560
	// MaxNormalizedImageBytes limits the final encoded payload sent to vision models.
	MaxNormalizedImageBytes = 280 << 10
	jpegQuality             = 90
)

var decodableImageMIMETypes = map[string]struct{}{
	"image/jpeg":          {},
	"image/png":           {},
	"image/gif":           {},
	"image/webp":          {},
	"image/heic":          {},
	"image/heif":          {},
	"image/heic-sequence": {},
	"image/heif-sequence": {},
}

// NormalizationResult describes how a raw image payload was normalized for the LLM provider.
type NormalizationResult struct {
	Image            llm.InputImage
	DetectedMIME     string
	DecodedFormat    string
	OriginalWidth    int
	OriginalHeight   int
	Width            int
	Height           int
	WasResized       bool
	NormalizedBytes  int
	Base64Chars      int
	PreservedAlpha   bool
	UsedDeclaredMIME bool
}

// LooksLikeImageMIME reports whether mimeType looks like an image attachment.
func LooksLikeImageMIME(mimeType string) bool {
	mimeType = normalizeMIMEType(mimeType)
	return strings.HasPrefix(mimeType, "image/")
}

// NormalizeInputImageFromBytes decodes a raw image, preserves alpha with PNG,
// and otherwise re-encodes to JPEG before returning the LLM payload.
func NormalizeInputImageFromBytes(header http.Header, declaredMIME string, data []byte, source string) (NormalizationResult, error) {
	if len(data) == 0 {
		return NormalizationResult{}, fmt.Errorf("image payload is empty")
	}
	if len(data) > MaxImageBytes {
		return NormalizationResult{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	detectedMIME, usedDeclaredMIME := DetectSourceMIMEType(header, declaredMIME, data)
	if detectedMIME == "" {
		return NormalizationResult{}, fmt.Errorf("unsupported or unknown image format")
	}

	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return NormalizationResult{}, fmt.Errorf("image payload decode failed for MIME type %q: %w", detectedMIME, err)
	}
	originalBounds := decoded.Bounds()
	originalWidth := originalBounds.Dx()
	originalHeight := originalBounds.Dy()
	isGIFContactSheet := false
	if strings.EqualFold(format, "gif") {
		decoded, originalWidth, originalHeight, isGIFContactSheet, err = decodeGIFContactSheet(data)
		if err != nil {
			return NormalizationResult{}, fmt.Errorf("image payload decode failed for MIME type %q: %w", detectedMIME, err)
		}
	}

	if originalWidth <= 0 || originalHeight <= 0 {
		return NormalizationResult{}, fmt.Errorf("image payload has invalid dimensions for MIME type %q", detectedMIME)
	}

	normalizedImage, wasResized, normalizedBytes, normalizedMIME, err := normalizeEncodedImage(decoded, MaxNormalizedImageLongEdge, MaxNormalizedImageBytes)
	if err != nil {
		return NormalizationResult{}, err
	}
	normalizedBounds := normalizedImage.Bounds()
	normalizedWidth := normalizedBounds.Dx()
	normalizedHeight := normalizedBounds.Dy()
	hasAlpha := hasTransparency(normalizedImage)
	encoded := base64.StdEncoding.EncodeToString(normalizedBytes)

	return NormalizationResult{
		Image: llm.InputImage{
			MimeType:          normalizedMIME,
			Data:              encoded,
			Source:            source,
			IsGIFContactSheet: isGIFContactSheet,
		},
		DetectedMIME:     detectedMIME,
		DecodedFormat:    strings.TrimSpace(strings.ToLower(format)),
		OriginalWidth:    originalWidth,
		OriginalHeight:   originalHeight,
		Width:            normalizedWidth,
		Height:           normalizedHeight,
		WasResized:       wasResized,
		NormalizedBytes:  len(normalizedBytes),
		Base64Chars:      len(encoded),
		PreservedAlpha:   hasAlpha,
		UsedDeclaredMIME: usedDeclaredMIME,
	}, nil
}

func decodeGIFContactSheet(data []byte) (image.Image, int, int, bool, error) {
	animation, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("decode GIF animation: %w", err)
	}
	if len(animation.Image) == 0 {
		return nil, 0, 0, false, fmt.Errorf("GIF contains no frames")
	}

	width := animation.Config.Width
	height := animation.Config.Height
	if width <= 0 || height <= 0 {
		bounds := animation.Image[0].Bounds()
		width = bounds.Max.X
		height = bounds.Max.Y
	}
	if width <= 0 || height <= 0 {
		return nil, 0, 0, false, fmt.Errorf("GIF has invalid dimensions")
	}
	if len(animation.Image) == 1 {
		return animation.Image[0], width, height, false, nil
	}

	selected := gifSampleFrameIndexes(animation.Delay, len(animation.Image))
	selectedSet := make(map[int]struct{}, len(selected))
	for _, index := range selected {
		selectedSet[index] = struct{}{}
	}

	canvasBounds := image.Rect(0, 0, width, height)
	canvas := image.NewNRGBA(canvasBounds)
	snapshots := make([]*image.NRGBA, 0, len(selected))
	for index, frame := range animation.Image {
		var previous *image.NRGBA
		if gifDisposal(animation, index) == gif.DisposalPrevious {
			previous = cloneNRGBA(canvas)
		}

		frameBounds := frame.Bounds().Intersect(canvasBounds)
		imagedraw.Draw(canvas, frameBounds, frame, frameBounds.Min, imagedraw.Over)
		if _, ok := selectedSet[index]; ok {
			snapshots = append(snapshots, cloneNRGBA(canvas))
		}

		switch gifDisposal(animation, index) {
		case gif.DisposalBackground:
			imagedraw.Draw(canvas, frameBounds, image.Transparent, image.Point{}, imagedraw.Src)
		case gif.DisposalPrevious:
			canvas = previous
		}
	}

	frames := make([]image.Image, len(snapshots))
	for index, snapshot := range snapshots {
		frames[index] = snapshot
	}
	sheet := buildContactSheet(frames, width, height)
	return sheet, width, height, true, nil
}

func buildContactSheet(frames []image.Image, width, height int) *image.NRGBA {
	columns := min(2, len(frames))
	rows := (len(frames) + columns - 1) / columns
	sheet := image.NewNRGBA(image.Rect(0, 0, width*columns, height*rows))
	for index, frame := range frames {
		offset := image.Pt(index%columns*width, index/columns*height)
		destination := image.Rectangle{Min: offset, Max: offset.Add(image.Pt(width, height))}
		imagedraw.Draw(sheet, destination, frame, frame.Bounds().Min, imagedraw.Src)
	}
	return sheet
}

func gifSampleFrameIndexes(delays []int, frameCount int) []int {
	if frameCount <= 4 {
		indexes := make([]int, frameCount)
		for index := range indexes {
			indexes[index] = index
		}
		return indexes
	}

	totalDelay := 0
	frameDelays := make([]int, frameCount)
	for index := range frameDelays {
		delay := 1
		if index < len(delays) && delays[index] > 0 {
			delay = delays[index]
		}
		frameDelays[index] = delay
		totalDelay += delay
	}

	indexes := []int{0, frameCount - 1}
	for _, target := range []int{totalDelay / 3, totalDelay * 2 / 3} {
		elapsed := 0
		for index, delay := range frameDelays {
			elapsed += delay
			if target < elapsed {
				indexes = append(indexes, index)
				break
			}
		}
	}
	sort.Ints(indexes)
	unique := indexes[:0]
	for _, index := range indexes {
		if len(unique) == 0 || unique[len(unique)-1] != index {
			unique = append(unique, index)
		}
	}
	return unique
}

func gifDisposal(animation *gif.GIF, index int) byte {
	if index < len(animation.Disposal) {
		return animation.Disposal[index]
	}
	return gif.DisposalNone
}

func cloneNRGBA(source *image.NRGBA) *image.NRGBA {
	clone := image.NewNRGBA(source.Bounds())
	copy(clone.Pix, source.Pix)
	return clone
}

// ResizeInputImagesForAttempt skips the initial resize for images already at or below maxLongEdge.
func ResizeInputImagesForAttempt(images []llm.InputImage, attempt int, retryScale float64, maxLongEdge int) ([]llm.InputImage, error) {
	return resizeInputImages(images, func(decoded image.Image) float64 {
		exponent := attempt
		bounds := decoded.Bounds()
		if max(bounds.Dx(), bounds.Dy()) <= maxLongEdge {
			exponent--
		}
		return math.Pow(retryScale, float64(exponent))
	})
}

func resizeInputImages(images []llm.InputImage, scaleFor func(image.Image) float64) ([]llm.InputImage, error) {
	resized := make([]llm.InputImage, len(images))
	for i, input := range images {
		data, err := base64.StdEncoding.DecodeString(base64Payload(input.Data))
		if err != nil {
			return nil, fmt.Errorf("decode normalized image %d: %w", i+1, err)
		}
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("decode normalized image %d: %w", i+1, err)
		}
		scale := scaleFor(decoded)
		if scale <= 0 || scale > 1 {
			return nil, fmt.Errorf("image scale must be greater than 0 and at most 1")
		}
		if scale == 1 {
			resized[i] = input
			continue
		}
		bounds := decoded.Bounds()
		width := max(1, int(float64(bounds.Dx())*scale+0.5))
		height := max(1, int(float64(bounds.Dy())*scale+0.5))
		dst := image.NewNRGBA(image.Rect(0, 0, width, height))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), decoded, bounds, xdraw.Over, nil)
		encoded, mimeType, err := encodeNormalizedImage(dst, hasTransparency(decoded))
		if err != nil {
			return nil, fmt.Errorf("encode resized image %d: %w", i+1, err)
		}
		resized[i] = llm.InputImage{
			MimeType:          mimeType,
			Data:              base64.StdEncoding.EncodeToString(encoded),
			Source:            input.Source,
			IsGIFContactSheet: input.IsGIFContactSheet,
		}
	}
	return resized, nil
}

func normalizeEncodedImage(img image.Image, maxLongEdge int, maxBytes int) (image.Image, bool, []byte, string, error) {
	currentMaxEdge := maxLongEdge
	wasResized := false

	for {
		normalizedImage, resized := resizeToFit(img, currentMaxEdge)
		wasResized = wasResized || resized
		hasAlpha := hasTransparency(normalizedImage)
		normalizedBytes, normalizedMIME, err := encodeNormalizedImage(normalizedImage, hasAlpha)
		if err != nil {
			return nil, false, nil, "", err
		}
		if maxBytes <= 0 || len(normalizedBytes) <= maxBytes {
			return normalizedImage, wasResized, normalizedBytes, normalizedMIME, nil
		}

		bounds := normalizedImage.Bounds()
		longEdge := max(bounds.Dx(), bounds.Dy())
		if longEdge <= 1 {
			return nil, false, nil, "", fmt.Errorf("normalized image exceeds %d bytes at minimum dimensions", maxBytes)
		}

		shrink := 0.95 * sqrtRatio(maxBytes, len(normalizedBytes))
		nextMaxEdge := max(1, int(float64(longEdge)*shrink))
		if nextMaxEdge >= longEdge {
			nextMaxEdge = longEdge - 1
		}
		currentMaxEdge = nextMaxEdge
		wasResized = true
	}
}

func sqrtRatio(target, actual int) float64 {
	if target <= 0 || actual <= 0 {
		return 1
	}
	return math.Sqrt(float64(target) / float64(actual))
}

// DetectSourceMIMEType returns a decodable image MIME type derived from bytes,
// HTTP metadata, or the declared MIME as a fallback.
func DetectSourceMIMEType(header http.Header, declaredMIME string, data []byte) (string, bool) {
	if detected := detectSpecialImageMIME(data); detected != "" {
		return detected, false
	}
	if contentType := normalizeMIMEType(header.Get("Content-Type")); supportsSourceMIMEType(contentType) {
		return contentType, false
	}
	if detected := normalizeMIMEType(http.DetectContentType(data)); supportsSourceMIMEType(detected) {
		return detected, false
	}
	declaredMIME = normalizeMIMEType(declaredMIME)
	if supportsSourceMIMEType(declaredMIME) {
		return declaredMIME, true
	}
	return "", false
}

func supportsSourceMIMEType(mimeType string) bool {
	_, ok := decodableImageMIMETypes[normalizeMIMEType(mimeType)]
	return ok
}

func detectSpecialImageMIME(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	boxType := string(data[4:8])
	brand := string(data[8:12])
	if boxType != "ftyp" {
		return ""
	}
	switch brand {
	case "heic", "heix", "hevc", "hevx":
		return "image/heic"
	case "mif1", "heif", "msf1":
		return "image/heif"
	default:
		return ""
	}
}

func encodeNormalizedImage(img image.Image, preserveAlpha bool) ([]byte, string, error) {
	var buf bytes.Buffer
	if preserveAlpha {
		nrgba := toNRGBA(img)
		if err := png.Encode(&buf, nrgba); err != nil {
			return nil, "", fmt.Errorf("normalize image as PNG: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("normalize image as JPEG: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

func resizeToFit(img image.Image, maxLongEdge int) (image.Image, bool) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if maxLongEdge <= 0 || width <= maxLongEdge && height <= maxLongEdge {
		return img, false
	}

	scale := float64(maxLongEdge) / float64(width)
	if height > width {
		scale = float64(maxLongEdge) / float64(height)
	}
	newWidth := max(1, int(float64(width)*scale+0.5))
	newHeight := max(1, int(float64(height)*scale+0.5))
	resized := image.NewNRGBA(image.Rect(0, 0, newWidth, newHeight))
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), img, bounds, xdraw.Over, nil)
	return resized, true
}

func hasTransparency(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0xffff {
				return true
			}
		}
	}
	return false
}

func toNRGBA(img image.Image) *image.NRGBA {
	bounds := img.Bounds()
	out := image.NewNRGBA(bounds)
	imagedraw.Draw(out, bounds, &image.Uniform{C: color.Transparent}, image.Point{}, imagedraw.Src)
	imagedraw.Draw(out, bounds, img, bounds.Min, imagedraw.Over)
	return out
}

func normalizeMIMEType(mimeType string) string {
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	return strings.TrimSpace(strings.ToLower(mimeType))
}

func base64Payload(encoded string) string {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return ""
	}
	if comma := strings.Index(trimmed, ","); comma >= 0 && strings.HasPrefix(trimmed[:comma], "data:") {
		return trimmed[comma+1:]
	}
	return trimmed
}
