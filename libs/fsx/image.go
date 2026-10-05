package fsx

import (
	"bytes"
	"fmt"
	"image"

	"github.com/veypi/aic-pod/libs/imageutil"
)

// §2.2 图片编解码（阈值/算法/格式判定）实现单一来源 = libs/imageutil，
// 本包只保留 read 管线形态（imageResult/imageContent/尺寸读取）。

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
	dataURI, compressedNote, err := imageutil.EncodeImageData(data, mime)
	if err != nil {
		return nil, fsErr("read", "image too large even after compression (%d bytes)", len(data))
	}
	if compressedNote != "" {
		r.Attrs["image_compressed"] = compressedNote
	}
	r.Attrs["image_data"] = dataURI
	r.Content = imageContent(abs, mime, w, h, len(data))
	return r, nil
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
