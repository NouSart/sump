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

// TestStoreTwoImagesIndependent 存两张不同的图：按 id 取回各自的内容。
//
// 这条盯的是"固定文件名"那类改动：落盘路径一旦不用 id（比如写死 img.bin），第二张会覆盖
// 第一张，而 Load 仍然成功——**静默的图片错乱**比报错难查得多。
func TestStoreTwoImagesIndependent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := genPNG(t)
	second := []byte("\x89PNG\r\n\x1a\n-第二张（不是真 PNG，只要字节不同就够）")
	id1, err := st.Save(first, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.Save(second, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("两次保存必须得到不同 id")
	}

	got1, _, err := st.Load(id1)
	if err != nil {
		t.Fatal(err)
	}
	got2, _, err := st.Load(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, first) {
		t.Fatal("第一张图被第二张覆盖了")
	}
	if !bytes.Equal(got2, second) {
		t.Fatal("第二张图内容不符")
	}
}
