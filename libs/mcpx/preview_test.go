package mcpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vsh/commands"
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

func previewCommandFixture(t *testing.T, result *mcp.CallToolResult) (context.Context, commands.CommandFunc, *int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	calls := 0
	server := mcp.NewServer(&mcp.Implementation{Name: "preview-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "capture", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if _, ok := args["image-preview"]; ok {
			t.Error("preview flag leaked into upstream arguments")
		}
		return result, nil
	})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, Command(func(context.Context, string) (*mcp.ClientSession, error) { return cs, nil }), &calls
}

func TestCommandImagePreviewIsExplicitAndPreservesCaptureMetadata(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		width, height         int
		preview               bool
		wantWidth, wantHeight int
	}{
		{name: "default passthrough", width: 64, height: 48},
		{name: "small preview without upscaling", width: 64, height: 48, preview: true, wantWidth: 64, wantHeight: 48},
		{name: "4K preview", width: 3840, height: 2160, preview: true, wantWidth: 1280, wantHeight: 720},
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
			ctx, command, calls := previewCommandFixture(t, original)
			args := []string{"call", "fixture", "capture", "--json"}
			if tc.preview {
				args = append(args, "--image-preview", "1280x720")
			}
			var stdout, stderr bytes.Buffer
			if err := command(ctx, &commands.Invocation{Args: args, Stdout: &stdout, Stderr: &stderr}); err != nil {
				t.Fatalf("%v: %s", err, &stderr)
			}
			if *calls != 1 {
				t.Fatalf("tool calls = %d, want 1", *calls)
			}
			var got mcp.CallToolResult
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
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
			if !tc.preview {
				if img.MIMEType != "image/png" || !bytes.Equal(img.Data, pngData) || len(img.Meta) != 1 {
					t.Fatal("default call transformed the image")
				}
				return
			}
			if img.MIMEType != "image/jpeg" || len(img.Data) > 600*1024 {
				t.Fatalf("preview format/size: %s %d", img.MIMEType, len(img.Data))
			}
			if stdout.Len() >= 1<<20 {
				t.Fatalf("preview JSON exceeds 1 MiB: %d bytes", stdout.Len())
			}
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(img.Data))
			if err != nil || cfg.Width != tc.wantWidth || cfg.Height != tc.wantHeight {
				t.Fatalf("preview geometry = %+v, %v", cfg, err)
			}
			meta, ok := img.Meta[imagePreviewMetaKey].(map[string]any)
			if !ok || meta["source_width"] != float64(tc.width) || meta["source_height"] != float64(tc.height) || meta["width"] != float64(tc.wantWidth) || meta["height"] != float64(tc.wantHeight) {
				t.Fatalf("preview metadata = %v", meta)
			}
		})
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

func TestCommandImagePreviewRejectsInvalidOptionsBeforeResolve(t *testing.T) {
	command := Command(func(context.Context, string) (*mcp.ClientSession, error) {
		t.Fatal("invalid preview option resolved service")
		return nil, nil
	})
	cases := [][]string{
		{"call", "fixture", "capture", "--image-preview"},
		{"call", "fixture", "capture", "--image-preview", "1280x720", "--image-preview", "1280x720"},
		{"tools", "fixture", "--image-preview", "1280x720"},
		{"describe", "fixture", "capture", "--image-preview", "1280x720"},
		{"read", "fixture", "result://saved", "--image-preview", "1280x720"},
	}
	for _, size := range []string{"", "0x720", "1280x0", "4097x720", "1280x4097", "-1x720", "+1x720", "1280X720", "1280x720x2", "1.5x720", "1280", "1280x 720"} {
		cases = append(cases, []string{"call", "fixture", "capture", "--image-preview", size})
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			err := command(context.Background(), &commands.Invocation{Args: args, Stdout: io.Discard, Stderr: &stderr})
			var exit *commands.ExitError
			if !errors.As(err, &exit) || exit.Code != 2 || !strings.Contains(stderr.String(), "--image-preview") {
				t.Fatalf("%v: %s", err, &stderr)
			}
		})
	}
}

func TestCommandImagePreviewFailureNeverRepeatsTool(t *testing.T) {
	for _, toolError := range []bool{false, true} {
		t.Run(map[bool]string{false: "conversion error", true: "upstream tool error"}[toolError], func(t *testing.T) {
			original := &mcp.CallToolResult{IsError: toolError, Content: []mcp.Content{&mcp.ImageContent{Data: []byte("invalid PNG"), MIMEType: "image/png"}}}
			ctx, command, calls := previewCommandFixture(t, original)
			var stdout, stderr bytes.Buffer
			err := command(ctx, &commands.Invocation{Args: []string{"call", "fixture", "capture", "--image-preview", "1280x720", "--json"}, Stdout: &stdout, Stderr: &stderr})
			var exit *commands.ExitError
			if !errors.As(err, &exit) || exit.Code != 1 || *calls != 1 {
				t.Fatalf("err=%v calls=%d", err, *calls)
			}
			if !toolError {
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), "tool completed but image preview failed") {
					t.Fatalf("conversion error = %q stdout bytes = %d", stderr.String(), stdout.Len())
				}
				return
			}
			var got mcp.CallToolResult
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !got.IsError || !bytes.Equal(got.Content[0].(*mcp.ImageContent).Data, []byte("invalid PNG")) || stderr.Len() != 0 {
				t.Fatal("upstream error result was transformed")
			}
		})
	}
}
