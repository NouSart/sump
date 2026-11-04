package server

// verify 长文档回归：片段跨块边界时仍要命中，不能因为分块读取给出假结论。

import (
	"context"
	"strings"
	"testing"

	"github.com/NouSart/sump/files/docmodel"
)

// chunkSource 是只实现 ReadRange 语义的最小文档（verifyFragments 只用它）。
type chunkSource struct{ text string }

func (d chunkSource) Kind() docmodel.Kind { return docmodel.KindText }
func (d chunkSource) Outline(context.Context, int) (docmodel.Outline, error) {
	return docmodel.Outline{}, nil
}
func (d chunkSource) Find(context.Context, docmodel.Query) ([]docmodel.Match, error) {
	return nil, nil
}
func (d chunkSource) ReadRange(_ context.Context, sel docmodel.Selector) (docmodel.Chunk, error) {
	rs := []rune(d.text)
	total := len(rs)
	offset := sel.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	limit := sel.Limit
	if limit <= 0 {
		limit = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	chunk := docmodel.Chunk{Text: string(rs[offset:end]), Total: total}
	if end < total {
		chunk.Next = end
	}
	return chunk, nil
}
func (d chunkSource) Patch(context.Context, []docmodel.PatchOp) (docmodel.PatchResult, error) {
	return docmodel.PatchResult{}, nil
}
func (d chunkSource) Bytes() []byte       { return []byte(d.text) }
func (d chunkSource) Digest() string      { return docmodel.Fingerprint(d.text) }
func (d chunkSource) ExactDigest() string { return docmodel.ExactFingerprint(d.text) }

// TestVerifyFragmentsAcrossChunkBoundary 片段正好跨过 verifyChunkRunes 边界。
func TestVerifyFragmentsAcrossChunkBoundary(t *testing.T) {
	needle := "xNEEDLE"
	text := strings.Repeat("x", verifyChunkRunes-1) + "NEEDLE" + strings.Repeat("y", 100)
	doc := chunkSource{text: text}

	missing, present, err := verifyFragments(context.Background(), doc,
		[]string{needle, "不存在的片段"},
		[]string{"yNEEDLE", "也不存在的片段"})
	if err != nil {
		t.Fatalf("verifyFragments 失败: %v", err)
	}
	if missing[0] {
		t.Fatalf("跨块片段 %q 被误判为缺失（chunk=%d）", needle, verifyChunkRunes)
	}
	if !missing[1] {
		t.Fatal("不存在片段应判缺失")
	}
	// yNEEDLE 里 y 在 NEEDLE 后，不是实际文本：不应命中。
	if present[0] {
		t.Fatal("不存在的 not_contains 片段被误判为仍有")
	}
	if present[1] {
		t.Fatal("不存在的 not_contains 片段被误判为仍有")
	}
}
