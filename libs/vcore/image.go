package vcore

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

// imageDataMaxBytes 是 image_data 的原始字节投递标准（§2.2：两端压缩阈值
// 对齐 600KB，算法一致——阶梯降质 → 逐级缩尺寸，输出 JPEG）。
// host/page 端提前压到投递标准，服务端落盘转换不再二次压缩。
const imageDataMaxBytes = 600 * 1024

// imageResult 生成可展示图片的 read 结果（§2.2 图片标准）：
//   - cloud（env.ImageData=false）：图片本就在 UFS，直接返回 image_path；
//   - host/page（env.ImageData=true）：返回 image_data（data URI，自包含），
//     超 600KB 自动压缩为 jpeg 并设置 image_compressed。
func imageResult(env *Env, abs string, data []byte, mime string) (*Result, error) {
	r := newResult("read", abs)
	r.Attrs["mime"] = mime
	r.set("size", len(data))
	w, h := imageDimensions(data)
	if !env.ImageData {
		r.Attrs["image_path"] = abs
		r.Content = imageContent(abs, mime, w, h, len(data))
		return r, nil
	}
	out, compressedNote := data, ""
	if len(data) > imageDataMaxBytes {
		c, err := compressImage(data, mime)
		if err != nil {
			return nil, fsErr("read", "image too large even after compression (%d bytes)", len(data))
		}
		out = c.data
		compressedNote = fmt.Sprintf("%d bytes → image/jpeg %dx%d quality %d (%d bytes)",
			len(data), c.width, c.height, c.quality, len(c.data))
		r.Attrs["image_compressed"] = compressedNote
	}
	r.Attrs["image_data"] = fmt.Sprintf("data:%s;base64,%s",
		pickMIME(mime, compressedNote != ""), base64.StdEncoding.EncodeToString(out))
	r.Content = imageContent(abs, mime, w, h, len(data))
	return r, nil
}

func pickMIME(orig string, compressed bool) string {
	if compressed {
		return "image/jpeg"
	}
	return orig
}

func imageContent(abs, mime string, w, h, size int) string {
	if w > 0 {
		return fmt.Sprintf("Image file: %s (%s, %dx%d, %d bytes)", abs, mime, w, h, size)
	}
	return fmt.Sprintf("Image file: %s (%s, %d bytes)", abs, mime, size)
}

// imageDimensions 读取图片尺寸（仅解析头部，不解码像素）。
func imageDimensions(data []byte) (width, height int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

type compressedImage struct {
	data          []byte
	width, height int
	quality       int
}

// compressImage 将超限图片压缩到 imageDataMaxBytes 以内，输出统一为 JPEG。
// 先在原尺寸阶梯降低质量（80/60/40），仍超限则按 0.5 倍逐级缩小尺寸重试。
func compressImage(data []byte, mime string) (*compressedImage, error) {
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
			if buf.Len() <= imageDataMaxBytes {
				cb := cur.Bounds()
				return &compressedImage{buf.Bytes(), cb.Dx(), cb.Dy(), q}, nil
			}
		}
		scale *= 0.5
	}
	return nil, fmt.Errorf("image still exceeds %d bytes after downscaling", imageDataMaxBytes)
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
