package vtools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/vsh/commands"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func runImageResult(t *testing.T, result *mcp.CallToolResult, withAttrs bool) (string, *commands.ResultAttrs, error) {
	t.Helper()
	var out, errOut strings.Builder
	attrs := commands.NewResultAttrs()
	inv := &commands.Invocation{Args: []string{"shot"}, Stdout: &out, Stderr: &errOut}
	if withAttrs {
		inv = commands.NewInvocation(&commands.InvocationOptions{
			Args: []string{"shot"}, Stdout: &out, Stderr: &errOut, ResultAttrs: attrs,
		})
	}
	p := newFixture(t)
	p.result = result
	err := RunProvider(context.Background(), inv, "fixture", p)
	return out.String(), attrs, err
}

func TestMCPImageMovesToResultAttrs(t *testing.T) {
	data := testPNG(t)
	encoded := base64.StdEncoding.EncodeToString(data)
	original := &mcp.CallToolResult{
		Meta:              mcp.Meta{"request": "original"},
		StructuredContent: map[string]any{"capture_id": "capture-1"},
		Content: []mcp.Content{
			&mcp.TextContent{Text: "captured"},
			&mcp.ImageContent{Data: data, MIMEType: "image/png", Meta: mcp.Meta{"upstream": "preserved"}},
		},
	}
	out, attrs, err := runImageResult(t, original, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, encoded) {
		t.Fatal("stdout still contains image base64")
	}
	dataURI := attrs.Snapshot()["image_data"]
	if !strings.HasPrefix(dataURI, "data:image/png;base64,") {
		t.Fatalf("image_data = %q", dataURI)
	}
	var got mcp.CallToolResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	structured := got.StructuredContent.(map[string]any)
	if structured["capture_id"] != "capture-1" || got.Meta["request"] != "original" {
		t.Fatalf("result metadata changed: %+v", got)
	}
	if got.Content[0].(*mcp.TextContent).Text != "captured" {
		t.Fatal("text content changed")
	}
	image := got.Content[1].(*mcp.ImageContent)
	if len(image.Data) != 0 || image.Meta["upstream"] != "preserved" {
		t.Fatalf("image was not detached cleanly: %+v", image)
	}
	marker := image.Meta[imageDataMetaKey].(map[string]any)
	if marker["attr"] != "image_data" {
		t.Fatalf("image marker = %#v", marker)
	}
	if len(original.Content[1].(*mcp.ImageContent).Data) == 0 {
		t.Fatal("upstream result was mutated")
	}
}

func TestMCPMultipleImagesUseImageDatas(t *testing.T) {
	data := testPNG(t)
	result := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: data, MIMEType: "image/png"},
		&mcp.ImageContent{Data: data, MIMEType: "image/png"},
	}}
	out, attrs, err := runImageResult(t, result, true)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	if err := json.Unmarshal([]byte(attrs.Snapshot()["image_datas"]), &uris); err != nil || len(uris) != 2 {
		t.Fatalf("image_datas = %q, %v", attrs.Snapshot()["image_datas"], err)
	}
	var got mcp.CallToolResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	for i, content := range got.Content {
		marker := content.(*mcp.ImageContent).Meta[imageDataMetaKey].(map[string]any)
		if marker["attr"] != "image_datas" || marker["index"] != float64(i) {
			t.Fatalf("marker %d = %#v", i, marker)
		}
	}
}

func TestMCPImageRequiresResultAttrs(t *testing.T) {
	result := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: testPNG(t), MIMEType: "image/png"},
	}}
	out, _, err := runImageResult(t, result, false)
	if err == nil || !strings.Contains(err.Error(), "result attrs are unavailable") {
		t.Fatalf("error = %v", err)
	}
	if out != "" {
		t.Fatalf("image was rendered without attrs: %q", out)
	}
}

func TestMCPErrorResultAlsoDetachesImage(t *testing.T) {
	result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
		&mcp.ImageContent{Data: testPNG(t), MIMEType: "image/png"},
	}}
	var out strings.Builder
	attrs := commands.NewResultAttrs()
	inv := commands.NewInvocation(&commands.InvocationOptions{Args: []string{"shot", "--json"}, Stdout: &out, ResultAttrs: attrs})
	p := newFixture(t)
	p.result = result
	p.callErr = &CallError{Result: result}
	err := RunProvider(context.Background(), inv, "fixture", p)
	if exitCode(err) != 1 || attrs.Snapshot()["image_data"] == "" || strings.Contains(out.String(), base64.StdEncoding.EncodeToString(testPNG(t))) {
		t.Fatalf("error result was not detached: out=%q attrs=%v err=%v", out.String(), attrs.Snapshot(), err)
	}
}
