package assemble

import (
	"strings"
	"testing"
)

// TestImagesRenderAsTextLines 覆盖 S-001 §3.3 / R-002 §5.2：只渲染 gist 文本，
// 首次带图片 id，之后只留 gist 行，lost 追加标记，绝不内联字节。
func TestImagesRenderAsTextLines(t *testing.T) {
	in := testInput()
	in.Images = []ImageRef{
		{ID: "img-1", Gist: "猫在键盘上", Status: "stored", First: true},
		{ID: "img-2", Gist: "狗", Status: "lost", First: true},
		{ID: "img-1", Gist: "猫在键盘上", Status: "stored", First: false},
		{ID: "img-3", Gist: "鸟", Status: "lost", First: false},
	}
	out := mustAssemble(t, in)

	want := []string{
		"[图片 img-1：猫在键盘上]",
		"[图片 img-2：狗（原图已丢失）]",
		"[图片：猫在键盘上]",
		"[图片：鸟（原图已丢失）]",
	}
	if len(out.Messages) != 1+len(want) {
		t.Fatalf("图片应逐张渲染成 user 行，实际 %d 条消息: %+v", len(out.Messages), out.Messages)
	}
	for i, w := range want {
		m := out.Messages[i+1]
		if m.Role != "user" || m.Content != w {
			t.Errorf("messages[%d] = {%s %q}，want user %q", i+1, m.Role, m.Content, w)
		}
		if strings.Contains(m.Content, "base64") {
			t.Errorf("纯函数不内联图片字节: %q", m.Content)
		}
	}
	// 非首见不重复完整行（只出现一次 “[图片 img-1：”）。
	full := 0
	for _, m := range out.Messages {
		if strings.Contains(m.Content, "[图片 img-1：") {
			full++
		}
	}
	if full != 1 {
		t.Fatalf("同一图片的完整行只该出现一次，实际 %d 次", full)
	}
	if got := findProv(out, "image"); len(got) != 4 {
		t.Fatalf("每张图应有一条 provenance: %+v", got)
	}
}

func TestImageEmptyGistShowsNotRecorded(t *testing.T) {
	in := testInput()
	in.Images = []ImageRef{{ID: "img-1", Gist: "  ", First: true}}
	out := mustAssemble(t, in)
	if got := out.Messages[1].Content; got != "[图片 img-1：未记录]" {
		t.Fatalf("空 gist 应显示“未记录”（不编造），得到 %q", got)
	}
}

func TestImagesDisabled(t *testing.T) {
	in := testInput()
	in.Config.IncludeImages = false
	in.Images = []ImageRef{{ID: "img-1", Gist: "猫", First: true}}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("IncludeImages=false 时不该渲染图片行: %+v", out.Messages)
	}
	if got := findProv(out, "image"); len(got) != 0 {
		t.Fatalf("不带图是配置决定，不该产生 image provenance: %+v", got)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("不带图不该产生 warning: %+v", out.Warnings)
	}
}

// TestImagesFollowHistory 固定块顺序：历史在前，图片（当前轮附件）在后。
func TestImagesFollowHistory(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "看这张图", WorkID: "w1", Seq: 1},
	}
	in.Images = []ImageRef{{ID: "img-1", Gist: "报错截图", First: true}}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("应得到 system + history + image，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Content != "看这张图" || out.Messages[2].Content != "[图片 img-1：报错截图]" {
		t.Fatalf("顺序应为 历史 → 图片: %+v", out.Messages)
	}
}
