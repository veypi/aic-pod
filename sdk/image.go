package aichost

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// imageDataMaxBytes 是 image_data 附件的原始字节上限。
// env 用户 NATS 单消息默认上限 1MB，base64 编码膨胀约 4/3，需为消息信封预留空间。
const imageDataMaxBytes = 1 << 20 // 1MB（§2.5）

// isViewableImage 判断图片格式是否可被模型直接查看，
// 与服务端 sessionctx.decodeBase64Image 支持的格式一致（其他格式默认按 .png 存储会损坏内容）。
func isViewableImage(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// imageDimensions 读取图片尺寸（仅解析头部，不解码像素）。
func imageDimensions(data []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// compressedImage 描述压缩后的图片结果。
type compressedImage struct {
	data    []byte
	width   int
	height  int
	quality int
}

// compressImage 将超限图片压缩到 imageDataMaxBytes 以内，输出统一为 JPEG。
// 先在原尺寸阶梯降低质量（80/60/40），仍超限则按 0.5 倍逐级缩小尺寸重试。
func compressImage(data []byte, mimeType string) (*compressedImage, error) {
	img, err := decodeImage(data, mimeType)
	if err != nil {
		return nil, err
	}

	// JPEG 无透明通道，先铺白底
	b := img.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, b.Min, draw.Over)

	scale := 1.0
	for range 6 {
		cur := image.Image(flat)
		if scale < 1.0 {
			w := max(1, int(float64(b.Dx())*scale))
			h := max(1, int(float64(b.Dy())*scale))
			scaled := image.NewRGBA(image.Rect(0, 0, w, h))
			xdraw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), flat, flat.Bounds(), xdraw.Over, nil)
			cur = scaled
		}
		for _, q := range []int{80, 60, 40} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, cur, &jpeg.Options{Quality: q}); err != nil {
				return nil, err
			}
			if buf.Len() <= imageDataMaxBytes {
				cb := cur.Bounds()
				return &compressedImage{data: buf.Bytes(), width: cb.Dx(), height: cb.Dy(), quality: q}, nil
			}
		}
		scale *= 0.5
	}
	return nil, fmt.Errorf("image still exceeds %d bytes after downscaling", imageDataMaxBytes)
}

func decodeImage(data []byte, mimeType string) (image.Image, error) {
	r := bytes.NewReader(data)
	switch mimeType {
	case "image/png":
		return png.Decode(r)
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/gif":
		return gif.Decode(r)
	case "image/webp":
		return webp.Decode(r)
	}
	return nil, fmt.Errorf("unsupported image format: %s", mimeType)
}
