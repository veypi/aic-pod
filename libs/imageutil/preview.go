package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"

	xdraw "golang.org/x/image/draw"
)

// Preview carries the original screenshot coordinate space alongside a small
// JPEG. Callers can display fewer pixels without changing input coordinates.
type Preview struct {
	Data                      []byte
	Width, Height             int
	SourceWidth, SourceHeight int
}

// PreviewImage fits an image inside the requested box, without upscaling.
// Encoding happens before command output so original screenshot bytes never
// cross the device transport. The first quality is 70; the byte budget is also
// bounded for noisy screenshots that remain large after resizing.
func PreviewImage(data []byte, mime string, maxWidth, maxHeight int) (*Preview, error) {
	if maxWidth < 1 || maxHeight < 1 || maxWidth > 4096 || maxHeight > 4096 {
		return nil, fmt.Errorf("preview bounds must be between 1 and 4096 pixels")
	}
	if len(data) > 64<<20 {
		return nil, fmt.Errorf("preview image exceeds 64 MiB")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read preview image dimensions: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > 64_000_000/int64(cfg.Height) {
		return nil, fmt.Errorf("preview source exceeds 64 million pixels")
	}
	img, err := decodeImage(data, mime)
	if err != nil {
		return nil, err
	}
	box := img.Bounds()
	scale := min(1.0, float64(maxWidth)/float64(box.Dx()), float64(maxHeight)/float64(box.Dy()))
	w, h := max(1, int(math.Floor(float64(box.Dx())*scale))), max(1, int(math.Floor(float64(box.Dy())*scale)))
	for range 6 {
		preview := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(preview, preview.Bounds(), image.White, image.Point{}, draw.Src)
		xdraw.ApproxBiLinear.Scale(preview, preview.Bounds(), img, box, draw.Over, nil)
		for _, quality := range []int{70, 55, 40} {
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, preview, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if encoded.Len() <= ImageDataMaxBytes {
				return &Preview{Data: encoded.Bytes(), Width: w, Height: h, SourceWidth: box.Dx(), SourceHeight: box.Dy()}, nil
			}
		}
		w, h = max(1, w*3/4), max(1, h*3/4)
	}
	return nil, fmt.Errorf("preview image exceeds %d bytes after compression", ImageDataMaxBytes)
}
