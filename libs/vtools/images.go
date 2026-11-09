package vtools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/imageutil"
	"github.com/veypi/vsh/commands"
)

const imageDataMetaKey = "aic.dev/image-data"

// Keep a multi-image result below the NATS response ceiling with room for the
// surrounding MCP JSON and protocol envelope.
const maxImageDataURIBytes = 900 * 1024

type detachedImage struct {
	contentIndex int
	dataURI      string
}

// writeProviderResult moves MCP image bytes out of stdout into the generic
// execution-result attrs channel. Other content and structured data stay put.
func writeProviderResult(inv *commands.Invocation, value any, compact bool) error {
	result, ok := value.(*mcp.CallToolResult)
	if !ok || result == nil {
		return WriteResult(inv, value, compact)
	}
	detached, err := detachMCPImages(inv, result)
	if err != nil {
		return err
	}
	return WriteResult(inv, detached, compact)
}

func detachMCPImages(inv *commands.Invocation, result *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	var images []detachedImage
	totalURIBytes := 0
	for index, content := range result.Content {
		image, ok := content.(*mcp.ImageContent)
		if !ok || image == nil {
			continue
		}
		if !imageutil.IsViewableImageMime(image.MIMEType) {
			return nil, fmt.Errorf("MCP image content %d has unsupported MIME type %q", index, image.MIMEType)
		}
		dataURI, _, err := imageutil.EncodeImageData(image.Data, image.MIMEType)
		if err != nil {
			return nil, fmt.Errorf("MCP image content %d: %w", index, err)
		}
		totalURIBytes += len(dataURI)
		if totalURIBytes > maxImageDataURIBytes {
			return nil, fmt.Errorf("MCP image data exceeds %d bytes after compression", maxImageDataURIBytes)
		}
		images = append(images, detachedImage{contentIndex: index, dataURI: dataURI})
	}
	if len(images) == 0 {
		return result, nil
	}

	attr := "image_data"
	if len(images) > 1 {
		attr = "image_datas"
		raw, err := json.Marshal(imageDataURIs(images))
		if err != nil {
			return nil, err
		}
		if err := inv.SetResultAttr(attr, string(raw)); err != nil {
			return nil, fmt.Errorf("attach MCP images: %w", err)
		}
	} else if err := inv.SetResultAttr(attr, images[0].dataURI); err != nil {
		return nil, fmt.Errorf("attach MCP image: %w", err)
	}

	copyResult := *result
	copyResult.Content = append([]mcp.Content(nil), result.Content...)
	for imageIndex, detached := range images {
		original := result.Content[detached.contentIndex].(*mcp.ImageContent)
		image := *original
		image.Data = nil
		image.MIMEType = dataURIMime(detached.dataURI)
		image.Meta = make(mcp.Meta, len(original.Meta)+1)
		for key, value := range original.Meta {
			image.Meta[key] = value
		}
		marker := map[string]any{"attr": attr}
		if attr == "image_datas" {
			marker["index"] = imageIndex
		}
		image.Meta[imageDataMetaKey] = marker
		copyResult.Content[detached.contentIndex] = &image
	}
	return &copyResult, nil
}

func imageDataURIs(images []detachedImage) []string {
	out := make([]string, len(images))
	for i, image := range images {
		out[i] = image.dataURI
	}
	return out
}

func dataURIMime(dataURI string) string {
	meta, _, _ := strings.Cut(dataURI, ",")
	meta = strings.TrimPrefix(meta, "data:")
	mime, _, _ := strings.Cut(meta, ";")
	return mime
}
