// 图片查看窗口：缩略图与局部原图。
//
// 为什么需要它：把整张图原样转 base64 塞进上下文，一张 8MB 的截图就是 ~11MB 的文本，
// 一次就把窗口撑爆。但模型又确实需要"看清某一块"——所以给它一个**滑动窗口**：
// 不带参数只看缩略图（长边 min(原图, max_side)），要看细节就传矩形（原图坐标），
// 拿回那一块的原图（必要时等比缩小到上限）。
//
// 坐标一律用**原图像素**：模型从返回的 original_width/height 里读尺寸，凭 x1,y1,x2,y2
// 平移窗口，不用在两套坐标系之间换算。
package server

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
)

const (
	// defaultMaxSide 是缩略图的默认长边上限（像素）。
	defaultMaxSide = 768
	// minMaxSide / maxMaxSide 夹住调用方给的 max_side：太小看不清，太大等于没限制。
	minMaxSide = 64
	maxMaxSide = 2048
	// maxResultBytes 是单次返回的二进制上限（base64 之后约 1.33 倍）。
	maxResultBytes = 768 << 10
	// jpegQuality 用于缩略图与局部图的重新编码。
	jpegQuality = 82
)

// errNotImage 表示数据不是可解码的图片（PNG / JPEG / GIF）。
var errNotImage = errors.New("不是可解码的图片（支持 PNG / JPEG / GIF）")

// decodeImage 解码图片；失败时返回 errNotImage（调用方据此决定要不要退回原样 base64）。
func decodeImage(data []byte) (image.Image, string, error) {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", errNotImage
	}
	return img, format, nil
}

// cropRect 把请求的矩形夹到原图范围内。
// 全 0 视为「没给矩形」；只给一半（例如只有 x1,y1）时把右下角补成原图边界。
func cropRect(img image.Image, x1, y1, x2, y2 int) (image.Rectangle, bool, error) {
	b := img.Bounds()
	if x1 == 0 && y1 == 0 && x2 == 0 && y2 == 0 {
		return b, false, nil // 没给矩形：整图
	}
	// 只给左上角时，右下角补成原图边界：模型常常只想说「从这儿往右下看」。
	if x2 == 0 {
		x2 = b.Max.X
	}
	if y2 == 0 {
		y2 = b.Max.Y
	}
	if x2 <= x1 || y2 <= y1 {
		return image.Rectangle{}, true, fmt.Errorf("矩形无效：需要 x2>x1 且 y2>y1（收到 %d,%d,%d,%d）", x1, y1, x2, y2)
	}
	r := image.Rect(x1, y1, x2, y2).Intersect(b)
	if r.Empty() {
		return image.Rectangle{}, true, fmt.Errorf("矩形落在图片外（图片 %dx%d，收到 %d,%d,%d,%d）",
			b.Dx(), b.Dy(), x1, y1, x2, y2)
	}
	return r, true, nil
}

// scaleToFit 把图片等比缩到长边不超过 maxSide；本来就不超就原样返回（scaled=false）。
func scaleToFit(img image.Image, maxSide int) (image.Image, bool) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= maxSide {
		return img, false
	}
	ratio := float64(maxSide) / float64(longest)
	nw := int(float64(w) * ratio)
	nh := int(float64(h) * ratio)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return resizeBilinear(img, nw, nh), true
}

// resizeBilinear 双线性缩放（不引第三方依赖；缩略图与局部图够用）。
func resizeBilinear(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 {
		return dst
	}
	// 半像素偏移：把目标像素中心映射回源坐标，避免整体偏移半个像素。
	scaleX := float64(sw) / float64(w)
	scaleY := float64(sh) / float64(h)
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*scaleY - 0.5
		y0 := int(fy)
		if fy < 0 {
			y0 = 0
			fy = 0
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		wy := fy - float64(y0)
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*scaleX - 0.5
			x0 := int(fx)
			if fx < 0 {
				x0 = 0
				fx = 0
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			wx := fx - float64(x0)
			c00 := toRGBA64(src.At(b.Min.X+x0, b.Min.Y+y0))
			c10 := toRGBA64(src.At(b.Min.X+x1, b.Min.Y+y0))
			c01 := toRGBA64(src.At(b.Min.X+x0, b.Min.Y+y1))
			c11 := toRGBA64(src.At(b.Min.X+x1, b.Min.Y+y1))
			dst.SetRGBA(x, y, color.RGBA{
				R: lerp(lerp(c00.R, c10.R, wx), lerp(c01.R, c11.R, wx), wy),
				G: lerp(lerp(c00.G, c10.G, wx), lerp(c01.G, c11.G, wx), wy),
				B: lerp(lerp(c00.B, c10.B, wx), lerp(c01.B, c11.B, wx), wy),
				A: lerp(lerp(c00.A, c10.A, wx), lerp(c01.A, c11.A, wx), wy),
			})
		}
	}
	return dst
}

type rgba64 struct{ R, G, B, A uint8 }

func toRGBA64(c color.Color) rgba64 {
	r, g, b, a := c.RGBA()
	return rgba64{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

func lerp(a, b uint8, t float64) uint8 {
	v := float64(a) + (float64(b)-float64(a))*t
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// cropImage 拷出矩形区域。
func cropImage(src image.Image, r image.Rectangle) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	// 从 src 的 Min 起算：At 用的是绝对坐标。
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			dst.Set(x, y, src.At(r.Min.X+x, r.Min.Y+y))
		}
	}
	return dst
}

// encodeFit 把图片编码成 PNG / JPEG，并在超出 maxResultBytes 时逐次缩小。
// 返回编码结果、最终尺寸、是否缩小过。
func encodeFit(img image.Image, preferJPEG bool) ([]byte, int, int, bool, error) {
	cur := img
	shrunk := false
	for attempt := 0; attempt < 5; attempt++ {
		out, err := encode(cur, preferJPEG)
		if err != nil {
			return nil, 0, 0, false, err
		}
		if len(out) <= maxResultBytes {
			return out, cur.Bounds().Dx(), cur.Bounds().Dy(), shrunk, nil
		}
		w := cur.Bounds().Dx()
		if w <= 64 {
			return nil, 0, 0, false, fmt.Errorf("局部太大（> %d KiB），请缩小矩形范围", maxResultBytes>>10)
		}
		cur = resizeBilinear(cur, w*3/4, cur.Bounds().Dy()*3/4)
		shrunk = true
	}
	return nil, 0, 0, false, fmt.Errorf("局部太大（> %d KiB），请缩小矩形范围", maxResultBytes>>10)
}

func encode(img image.Image, preferJPEG bool) ([]byte, error) {
	var buf bytes.Buffer
	if preferJPEG {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// 注册解码器：image.Decode 需要它们可用。
func init() {
	_ = gif.Decode
	_ = jpeg.Decode
	_ = png.Decode
}
