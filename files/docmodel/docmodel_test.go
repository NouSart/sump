package docmodel

import (
	"context"
	"errors"
	"testing"
)

// fakeDoc 是最小 Document 实现，只为验证注册表与类型契约。
type fakeDoc struct{}

func (fakeDoc) Kind() Kind { return Kind("fake-test") }
func (fakeDoc) Outline(ctx context.Context, depth int) (Outline, error) {
	return Outline{Kind: Kind("fake-test")}, nil
}
func (fakeDoc) Find(ctx context.Context, q Query) ([]Match, error) { return nil, ErrNoMatch }
func (fakeDoc) ReadRange(ctx context.Context, s Selector) (Chunk, error) {
	return Chunk{Text: "x", Total: 1}, nil
}
func (fakeDoc) Patch(ctx context.Context, ops []PatchOp) (PatchResult, error) {
	return PatchResult{}, nil
}
func (fakeDoc) Bytes() []byte  { return []byte("fake") }
func (fakeDoc) Digest() string { return Fingerprint("fake") }

// TestRegisterAndOpen 注册表是"格式实现 → 服务层"的唯一连接点，必须能取到、
// 未注册的类型要回 ErrUnsupported（服务层据此回可读错误）。
func TestRegisterAndOpen(t *testing.T) {
	Register(Kind("fake-test"), func(data []byte) (Document, error) { return fakeDoc{}, nil })
	doc, err := Open(Kind("fake-test"), []byte("x"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if doc.Kind() != Kind("fake-test") {
		t.Fatalf("Kind = %s", doc.Kind())
	}
	if _, err := Open(Kind("nope"), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("未注册类型应返回 ErrUnsupported，实际 %v", err)
	}
	found := false
	for _, k := range Kinds() {
		if k == Kind("fake-test") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Kinds 里没有刚注册的类型：%v", Kinds())
	}
	// 空 Kind / 空 Opener 不该污染注册表。
	Register("", func(data []byte) (Document, error) { return nil, nil })
	Register(Kind("nil-opener"), nil)
	if len(Kinds()) != 1 {
		t.Fatalf("注册表被无效注册污染：%v", Kinds())
	}
}

// TestFingerprint 锚点指纹要抗排版差异、认得内容差异。
func TestFingerprint(t *testing.T) {
	a := Fingerprint("甲方：上海某某科技有限公司")
	b := Fingerprint("\n  甲方：上海某某科技有限公司  \r\n")
	if a != b {
		t.Fatal("首尾空白差异不该让指纹变化")
	}
	if Fingerprint("A  B") != Fingerprint("A\tB\n") {
		t.Fatal("连续空白差异不该让指纹变化")
	}
	if a == Fingerprint("甲方：北京某某科技有限公司") {
		t.Fatal("内容不同却得到相同指纹")
	}
	if a == Fingerprint("甲方：上海某某科技有限公司。") {
		t.Fatal("标点变化应改变指纹")
	}
	if len(a) != 16 {
		t.Fatalf("指纹长度 = %d，期望 16", len(a))
	}
}

// TestNodeIDValid 地址形状校验：挡住"第 3 段"这类自由文本当地址用。
func TestNodeIDValid(t *testing.T) {
	cases := map[NodeID]bool{
		"body/p12":        true,
		"body/tbl3/r2/c1": true,
		"sheet1!C7":       true,
		"":                false,
		"第 3 段":           false,
	}
	for id, want := range cases {
		if got := id.Valid(); got != want {
			t.Fatalf("%q.Valid() = %v，期望 %v", id, got, want)
		}
	}
}
