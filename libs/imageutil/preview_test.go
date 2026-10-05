package imageutil

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"strings"
	"testing"
)

func TestPreviewImageFits720pAndPreservesCoordinates(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		width, height, wantWidth, wantHeight int
	}{
		{"4k desktop", 3840, 2160, 1280, 720},
		{"portrait window", 1080, 1920, 405, 720},
		{"small window", 160, 100, 160, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := noisyPNG(t, test.width, test.height)
			preview, err := PreviewImage(original, "image/png", 1280, 720)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Width != test.wantWidth || preview.Height != test.wantHeight || preview.SourceWidth != test.width || preview.SourceHeight != test.height {
				t.Fatalf("incorrect preview or coordinate dimensions: %dx%d from %dx%d", preview.Width, preview.Height, preview.SourceWidth, preview.SourceHeight)
			}
			if len(preview.Data) > ImageDataMaxBytes {
				t.Fatal("preview exceeded byte budget")
			}
			decoded, err := jpeg.Decode(bytes.NewReader(preview.Data))
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Bounds() != image.Rect(0, 0, preview.Width, preview.Height) {
				t.Fatal("reported preview dimensions differ from encoded JPEG")
			}
			t.Logf("%dx%d PNG %d bytes -> %dx%d JPEG %d bytes", test.width, test.height, len(original), preview.Width, preview.Height, len(preview.Data))
		})
	}
}

func TestPreviewImageRejectsInvalidAndOversizedInputs(t *testing.T) {
	raw := noisyPNG(t, 16, 16)
	for _, bounds := range [][2]int{{0, 720}, {1280, -1}, {4097, 720}} {
		if _, err := PreviewImage(raw, "image/png", bounds[0], bounds[1]); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
	if _, err := PreviewImage([]byte("not an image"), "image/png", 1280, 720); err == nil {
		t.Fatal("invalid image accepted")
	}
	if _, err := PreviewImage(raw, "application/octet-stream", 1280, 720); err == nil {
		t.Fatal("unsupported MIME accepted")
	}
	// A valid IHDR declaring excessive pixels must be rejected before decode
	// allocates a source bitmap. No large bitmap is needed for the fixture.
	binary.BigEndian.PutUint32(raw[16:20], 100000)
	binary.BigEndian.PutUint32(raw[20:24], 100000)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	if _, err := PreviewImage(raw, "image/png", 1280, 720); err == nil || !strings.Contains(err.Error(), "64 million pixels") {
		t.Fatalf("pixel limit: %v", err)
	}
}
