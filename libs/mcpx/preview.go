package mcpx

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/imageutil"
)

const imagePreviewMetaKey = "aic.dev/image-preview"

type imagePreviewSize struct {
	width, height int
}

func parseImagePreviewSize(value string) (*imagePreviewSize, error) {
	w, h, ok := strings.Cut(value, "x")
	parse := func(part string) int {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 4096 {
			return 0
		}
		return n
	}
	width, height := parse(w), parse(h)
	if !ok || width == 0 || height == 0 {
		return nil, fmt.Errorf("--image-preview requires WIDTHxHEIGHT with each dimension in 1..4096")
	}
	return &imagePreviewSize{width: width, height: height}, nil
}

// previewToolImages transforms only explicitly requested image content. Metadata
// from the upstream capture remains unchanged; source dimensions allow callers
// to undo this additional preview scaling before sending coordinates upstream.
func previewToolImages(result *mcp.CallToolResult, size imagePreviewSize) (*mcp.CallToolResult, error) {
	if result.IsError {
		return result, nil
	}
	copyResult := *result
	copyResult.Content = append([]mcp.Content(nil), result.Content...)
	for index, content := range result.Content {
		original, ok := content.(*mcp.ImageContent)
		if !ok || original == nil {
			continue
		}
		preview, err := imageutil.PreviewImage(original.Data, original.MIMEType, size.width, size.height)
		if err != nil {
			return nil, fmt.Errorf("tool completed but image preview failed for content %d: %w", index, err)
		}
		image := *original
		image.Data, image.MIMEType = preview.Data, "image/jpeg"
		image.Meta = make(mcp.Meta, len(original.Meta)+1)
		for key, value := range original.Meta {
			image.Meta[key] = value
		}
		image.Meta[imagePreviewMetaKey] = map[string]any{
			"source_width": preview.SourceWidth, "source_height": preview.SourceHeight,
			"width": preview.Width, "height": preview.Height,
		}
		copyResult.Content[index] = &image
	}
	return &copyResult, nil
}
