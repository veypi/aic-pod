package mcpx

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func previewPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 30, G: 90, B: 180, A: 255}), image.Point{}, draw.Src)
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestPreviewToolImagesIsExplicitAndPreservesCaptureMetadata(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		width, height         int
		wantWidth, wantHeight int
	}{
		{name: "small preview without upscaling", width: 64, height: 48, wantWidth: 64, wantHeight: 48},
		{name: "4K preview", width: 3840, height: 2160, wantWidth: 1280, wantHeight: 720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pngData := previewPNG(t, tc.width, tc.height)
			original := &mcp.CallToolResult{Meta: mcp.Meta{"request": "original"}, StructuredContent: map[string]any{
				"capture_id": "capture-7", "screen_width": tc.width, "screen_height": tc.height, "scale_factor": 2,
			}, Content: []mcp.Content{
				&mcp.TextContent{Text: "captured"},
				&mcp.ImageContent{Data: pngData, MIMEType: "image/png", Meta: mcp.Meta{"upstream": "preserved"}, Annotations: &mcp.Annotations{Priority: 0.5}},
				&mcp.ResourceLink{URI: "capture://native", Name: "native"},
			}}
			got, err := PreviewToolImages(original, "1280x720")
			if err != nil {
				t.Fatal(err)
			}
			wantMetadata, _ := json.Marshal(original.StructuredContent)
			gotMetadata, _ := json.Marshal(got.StructuredContent)
			wantResultMeta, _ := json.Marshal(original.Meta)
			gotResultMeta, _ := json.Marshal(got.Meta)
			if !bytes.Equal(gotMetadata, wantMetadata) || !bytes.Equal(gotResultMeta, wantResultMeta) {
				t.Fatalf("capture metadata changed: got=%s want=%s result meta=%s want=%s", gotMetadata, wantMetadata, gotResultMeta, wantResultMeta)
			}
			if len(got.Content) != 3 || got.Content[0].(*mcp.TextContent).Text != "captured" || got.Content[2].(*mcp.ResourceLink).URI != "capture://native" {
				t.Fatal("non-image content changed")
			}
			img := got.Content[1].(*mcp.ImageContent)
			if img.Meta["upstream"] != "preserved" || !reflect.DeepEqual(img.Annotations, original.Content[1].(*mcp.ImageContent).Annotations) {
				t.Fatal("image annotations or metadata changed")
			}
			if img.MIMEType != "image/jpeg" || len(img.Data) > 600*1024 {
				t.Fatalf("preview format/size: %s %d", img.MIMEType, len(img.Data))
			}
			config, err := jpeg.DecodeConfig(bytes.NewReader(img.Data))
			if err != nil || config.Width != tc.wantWidth || config.Height != tc.wantHeight {
				t.Fatalf("preview geometry = %+v, %v", config, err)
			}
			meta, ok := img.Meta[imagePreviewMetaKey].(map[string]any)
			if !ok || meta["source_width"] != tc.width || meta["source_height"] != tc.height || meta["width"] != tc.wantWidth || meta["height"] != tc.wantHeight {
				t.Fatalf("preview metadata = %v", meta)
			}
		})
	}
}

func TestPreviewToolImagesRejectsInvalidSize(t *testing.T) {
	for _, size := range []string{"", "0x720", "1280x0", "4097x720", "1280x4097", "-1x720", "+1x720", "1280X720", "1280x720x2", "1.5x720", "1280", "1280x 720"} {
		if _, err := PreviewToolImages(&mcp.CallToolResult{}, size); err == nil {
			t.Fatalf("invalid size %q accepted", size)
		}
	}
}

func TestPreviewToolImagesDoesNotMutateUpstreamResult(t *testing.T) {
	originalImage := &mcp.ImageContent{Data: previewPNG(t, 32, 24), MIMEType: "image/png", Meta: mcp.Meta{"upstream": "value"}}
	original := &mcp.CallToolResult{Content: []mcp.Content{originalImage}, StructuredContent: map[string]any{"capture_id": "original"}}
	before, _ := json.Marshal(original)
	got, err := previewToolImages(original, imagePreviewSize{width: 1280, height: 720})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(original)
	if !bytes.Equal(before, after) || got == original || got.Content[0] == originalImage {
		t.Fatal("preview mutated or reused upstream result/image")
	}
}

func TestPreviewToolImagesLeavesToolErrorsAlone(t *testing.T) {
	original := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.ImageContent{Data: []byte("invalid PNG"), MIMEType: "image/png"}}}
	got, err := PreviewToolImages(original, "1280x720")
	if err != nil || got != original {
		t.Fatalf("tool error result = %+v, %v; want original", got, err)
	}
}
