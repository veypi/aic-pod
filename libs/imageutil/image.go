package imageutil

// Shared image encoding and compression for device tools and cloud results.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// ImageDataMaxBytes 是 image_data 的原始字节投递标准（§2.2：压缩阈值
// 600KB，阶梯降质 → 逐级缩尺寸，输出 JPEG）。host/page 端提前压到投递
// 标准，服务端落盘转换不再二次压缩。
const ImageDataMaxBytes = 600 * 1024

// EncodeImageData 将图片字节编码为 §2.2 标准的 data URI（provider 把本地截图
// 等转成 image_data 附在工具返回里——服务端统一落盘投递）：超过
// ImageDataMaxBytes 自动压缩为 JPEG。返回 (dataURI, 压缩说明 note, err)，
// note 为空表示未压缩。
func EncodeImageData(data []byte, mime string) (string, string, error) {
	out, note := data, ""
	if len(data) > ImageDataMaxBytes {
		c, err := CompressImage(data, mime, ImageDataMaxBytes)
		if err != nil {
			return "", "", err
		}
		out = c.Data
		note = fmt.Sprintf("%d bytes → image/jpeg %dx%d quality %d (%d bytes)",
			len(data), c.Width, c.Height, c.Quality, len(c.Data))
	}
	return fmt.Sprintf("data:%s;base64,%s",
		pickMIME(mime, note != ""), base64.StdEncoding.EncodeToString(out)), note, nil
}

// IsViewableImageMime 判定图片格式是否可被模型直接查看（§2.2：png/jpg/gif/webp）。
func IsViewableImageMime(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func pickMIME(orig string, compressed bool) string {
	if compressed {
		return "image/jpeg"
	}
	return orig
}

// CompressedImage 是压缩后的图片结果（JPEG 字节 + 尺寸 + 质量档位）。
type CompressedImage struct {
	Data          []byte
	Width, Height int
	Quality       int
}

// CompressImage 将超限图片压缩到 maxBytes 以内，输出统一为 JPEG。
// 先在原尺寸阶梯降低质量（80/60/40），仍超限则按 0.5 倍逐级缩小尺寸重试。
func CompressImage(data []byte, mime string, maxBytes int) (*CompressedImage, error) {
	img, err := decodeImage(data, mime)
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
			if buf.Len() <= maxBytes {
				cb := cur.Bounds()
				return &CompressedImage{buf.Bytes(), cb.Dx(), cb.Dy(), q}, nil
			}
		}
		scale *= 0.5
	}
	return nil, fmt.Errorf("image still exceeds %d bytes after downscaling", maxBytes)
}

func decodeImage(data []byte, mime string) (image.Image, error) {
	r := bytes.NewReader(data)
	switch mime {
	case "image/png":
		return png.Decode(r)
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/gif":
		return gif.Decode(r)
	case "image/webp":
		return webp.Decode(r)
	}
	return nil, fmt.Errorf("unsupported image format: %s", mime)
}
