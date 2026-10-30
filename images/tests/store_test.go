package tests

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/images/store"
)

// genPNG 程序化生成一张 2x2 PNG（测试素材，避免外部文件依赖）。
func genPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStoreSaveLoad 验证落盘/元数据/读取与不存在场景。
func TestStoreSaveLoad(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	data := genPNG(t)
	id, err := st.Save(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("id 为空")
	}
	got, mime, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) || mime != "image/png" {
		t.Fatalf("读取不符: len=%d mime=%s", len(got), mime)
	}
	if _, _, err := st.Load("不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得到: %v", err)
	}
}
