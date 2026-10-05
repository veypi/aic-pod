package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// noisyPNG 生成确定性噪声 PNG（压不动，保证能触到阈值路径）。
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(7)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 小图原样透传（mime 不变、note 空）。
func TestEncodeImageDataSmallPassthrough(t *testing.T) {
	raw := noisyPNG(t, 32, 32)
	uri, note, err := EncodeImageData(raw, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if note != "" || !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("small image must pass through: note=%q uri=%.32s", note, uri)
	}
}

// 超限压缩：输出 JPEG、落进 maxBytes、note 非空。
func TestEncodeImageDataCompresses(t *testing.T) {
	raw := noisyPNG(t, 1024, 1024)
	if len(raw) <= ImageDataMaxBytes {
		t.Skipf("fixture too small (%d bytes)", len(raw))
	}
	uri, note, err := EncodeImageData(raw, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if note == "" || !strings.HasPrefix(uri, "data:image/jpeg;base64,") {
		t.Fatalf("big image must compress: note=%q uri=%.32s", note, uri)
	}
}

// CompressImage 契约：JPEG 输出、尺寸与质量有效、字节数不超上限。
func TestCompressImageFitsLimit(t *testing.T) {
	const maxBytes = 120 * 1024
	c, err := CompressImage(noisyPNG(t, 512, 512), "image/png", maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Data) > maxBytes || c.Width == 0 || c.Height == 0 || c.Quality == 0 {
		t.Fatalf("bad result: %d bytes %dx%d q%d", len(c.Data), c.Width, c.Height, c.Quality)
	}
	if !bytes.HasPrefix(c.Data, []byte{0xff, 0xd8}) {
		t.Fatal("output must be JPEG")
	}
}
