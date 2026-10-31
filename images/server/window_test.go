// 图片查看窗口的单元测试：矩形夹取、等比缩放、局部拷贝、编码上限。
//
// 这套逻辑决定「模型能看到哪一块、看多大」——坐标算错会让模型反复取错区域，
// 缩放算错会让细节糊掉，所以每个坐标边界都单独测。
package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// solidImage 造一张纯色图（便于按像素断言）。
func solidImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// quadImage 造一张四象限图：左上红、右上绿、左下蓝、右下白。
func quadImage(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	half := size / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var c color.RGBA
			switch {
			case x < half && y < half:
				c = color.RGBA{R: 255, A: 255}
			case x >= half && y < half:
				c = color.RGBA{G: 255, A: 255}
			case x < half && y >= half:
				c = color.RGBA{B: 255, A: 255}
			default:
				c = color.RGBA{R: 255, G: 255, B: 255, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestCropRect(t *testing.T) {
	img := solidImage(100, 80, color.RGBA{A: 255})

	for _, tc := range []struct {
		name           string
		x1, y1, x2, y2 int
		wantCropped    bool
		want           image.Rectangle
		wantErr        bool
	}{
		{name: "全 0 = 整图", want: image.Rect(0, 0, 100, 80)},
		{name: "正常矩形", x1: 10, y1: 20, x2: 40, y2: 60, wantCropped: true, want: image.Rect(10, 20, 40, 60)},
		{name: "只给左上角补到边界", x1: 30, y1: 30, wantCropped: true, want: image.Rect(30, 30, 100, 80)},
		{name: "右下越界被夹住", x1: 50, y1: 50, x2: 999, y2: 999, wantCropped: true, want: image.Rect(50, 50, 100, 80)},
		{name: "反方向矩形报错", x1: 40, y1: 40, x2: 10, y2: 10, wantCropped: true, wantErr: true},
		{name: "矩形完全在图外报错", x1: 200, y1: 200, x2: 300, y2: 300, wantCropped: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cropped, err := cropRect(img, tc.x1, tc.y1, tc.x2, tc.y2)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际得到 %v", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if cropped != tc.wantCropped {
				t.Fatalf("cropped = %v, want %v", cropped, tc.wantCropped)
			}
			if r != tc.want {
				t.Fatalf("矩形 = %v, want %v", r, tc.want)
			}
		})
	}
}

func TestScaleToFit(t *testing.T) {
	// 长边 2000 → 缩到 800，等比（1000x2000 → 400x800）。
	big := solidImage(1000, 2000, color.RGBA{R: 1, A: 255})
	out, scaled := scaleToFit(big, 800)
	if !scaled {
		t.Fatal("超长边应缩放")
	}
	if out.Bounds().Dx() != 400 || out.Bounds().Dy() != 800 {
		t.Fatalf("缩放尺寸 = %dx%d, want 400x800", out.Bounds().Dx(), out.Bounds().Dy())
	}

	// 本来就不超：原样返回，不浪费一次重编码。
	small := solidImage(100, 50, color.RGBA{G: 1, A: 255})
	out2, scaled2 := scaleToFit(small, 800)
	if scaled2 {
		t.Fatal("小图不该缩放")
	}
	if out2.Bounds().Dx() != 100 || out2.Bounds().Dy() != 50 {
		t.Fatalf("小图尺寸被改了: %v", out2.Bounds())
	}
}

// 局部拷贝必须取到「对的那一块」：四象限图里取右下 = 白。
func TestCropImageTakesTheRightQuadrant(t *testing.T) {
	img := quadImage(100)
	cases := []struct {
		name string
		r    image.Rectangle
		want color.RGBA
	}{
		{name: "左上红", r: image.Rect(5, 5, 45, 45), want: color.RGBA{R: 255, A: 255}},
		{name: "右上绿", r: image.Rect(55, 5, 95, 45), want: color.RGBA{G: 255, A: 255}},
		{name: "左下蓝", r: image.Rect(5, 55, 45, 95), want: color.RGBA{B: 255, A: 255}},
		{name: "右下白", r: image.Rect(55, 55, 95, 95), want: color.RGBA{R: 255, G: 255, B: 255, A: 255}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cropImage(img, tc.r)
			if got.Bounds().Dx() != tc.r.Dx() || got.Bounds().Dy() != tc.r.Dy() {
				t.Fatalf("尺寸 = %v, want %v", got.Bounds(), tc.r)
			}
			if c := toRGBA64(got.At(1, 1)); c != (rgba64{tc.want.R, tc.want.G, tc.want.B, tc.want.A}) {
				t.Fatalf("像素 = %+v, want %+v", c, tc.want)
			}
		})
	}
}

// 编码必须落在上限之内：造一张大噪声图，编码后应被逐次缩小到 maxResultBytes 以下。
func TestEncodeFitStaysUnderLimit(t *testing.T) {
	// 噪声图几乎不可压缩，最容易触发"太大再缩"。
	img := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
	seed := uint32(12345)
	for y := 0; y < 1200; y++ {
		for x := 0; x < 1200; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	out, w, h, shrunk, err := encodeFit(img, false)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if len(out) > maxResultBytes {
		t.Fatalf("编码结果 %d 字节，超过上限 %d", len(out), maxResultBytes)
	}
	if !shrunk {
		t.Fatal("大噪声图应被缩小过")
	}
	if w > 1200 || h > 1200 || w <= 0 || h <= 0 {
		t.Fatalf("缩放后尺寸不合理: %dx%d", w, h)
	}
}

// 非图片数据：解码失败要能被识别出来（调用方据此决定是否退回原样 base64）。
func TestDecodeImageRejectsNonImage(t *testing.T) {
	if _, _, err := decodeImage([]byte("这不是图片")); err != errNotImage {
		t.Fatalf("应返回 errNotImage，实际 %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, solidImage(8, 8, color.RGBA{A: 255})); err != nil {
		t.Fatal(err)
	}
	if _, format, err := decodeImage(buf.Bytes()); err != nil || format != "png" {
		t.Fatalf("合法 PNG 应解码成功: format=%q err=%v", format, err)
	}
}
