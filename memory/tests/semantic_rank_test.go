// 向量列与语义排序的单元测试：BLOB 往返（落库 → 读回）、换模型失效语义、RankSemantic 阈值与预算。
package tests

import (
	"testing"

	"github.com/NouSart/sump/memory/longterm"
)

func TestEmbeddingRoundTripAndModelTag(t *testing.T) {
	lt := openLT(t)
	id, _, err := lt.Add("偏好", "爱吃苹果", 0, "", "test")
	if err != nil {
		t.Fatal(err)
	}

	vec := []float32{0.25, -1, 2, 0.5}
	if err := lt.SetEmbedding(id, vec, "model-a"); err != nil {
		t.Fatal(err)
	}
	entries, err := lt.ListActive()
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListActive 不符: %+v err=%v", entries, err)
	}
	got := entries[0]
	if len(got.Embedding) != len(vec) || got.EmbeddingModel != "model-a" {
		t.Fatalf("向量读回不符: %+v", got)
	}
	for i := range vec {
		if got.Embedding[i] != vec[i] {
			t.Fatalf("第 %d 分量不符: %v != %v", i, got.Embedding[i], vec[i])
		}
	}

	// 覆盖写入（换模型）：同列可改写。
	if err := lt.SetEmbedding(id, []float32{1, 0}, "model-b"); err != nil {
		t.Fatal(err)
	}
	entries, _ = lt.ListActive()
	if len(entries[0].Embedding) != 2 || entries[0].EmbeddingModel != "model-b" {
		t.Fatalf("覆盖后不符: %+v", entries[0])
	}
}

func TestRankSemanticThresholdBudgetAndOrder(t *testing.T) {
	entries := []longterm.Entry{
		{ID: 1, Content: "一个很长的条目内容", Embedding: []float32{0.9, 0.1}}, // 高相似
		{ID: 2, Content: "中等", Embedding: []float32{0.5, 0.5}},        // ≈0.707
		{ID: 3, Content: "无关", Embedding: []float32{0, 1}},            // 0（低于阈值）
		{ID: 4, Content: "没算过向量"},                                     // 跳过
		{ID: 5, Content: "维度不符", Embedding: []float32{1, 0, 0}},       // 跳过
	}
	q := []float32{1, 0}

	// 阈值 0.45：只剩 1 与 2，按相似度降序。
	got := longterm.RankSemantic(entries, q, 5, 1000, 0.45)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("排序不符: %+v", ids(got))
	}
	// 阈值 0.75：只剩 1（0.707 被滤掉）。
	got = longterm.RankSemantic(entries, q, 5, 1000, 0.75)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("阈值过滤不符: %+v", ids(got))
	}
	// 条数预算：limit=1 → 只取最高。
	got = longterm.RankSemantic(entries, q, 1, 1000, 0.45)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("条数预算不符: %+v", ids(got))
	}
	// 字符预算：只允许第二条（"中等"，2 字）——第一条 9 字超预算被跳过。
	got = longterm.RankSemantic(entries, q, 5, 2, 0.45)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("字符预算不符: %+v", ids(got))
	}
	// 空查询向量：空结果。
	if got := longterm.RankSemantic(entries, nil, 5, 1000, 0.45); got != nil {
		t.Fatalf("空查询应为空: %+v", ids(got))
	}
}

// 相似度数值锚：cos([1,0],[0.5,0.5]) ≈ 0.7071（归一化兜底正确）。
func TestCosineNormalizationFallback(t *testing.T) {
	got := longterm.RankSemantic([]longterm.Entry{{ID: 1, Content: "x", Embedding: []float32{0.5, 0.5}}}, []float32{1, 0}, 5, 10, 0.0)
	if len(got) != 1 {
		t.Fatal("应命中")
	}
	// 通过阈值行为间接验证数值（0.7071 > 0.7 且 < 0.71）
	if len(longterm.RankSemantic([]longterm.Entry{{ID: 1, Content: "x", Embedding: []float32{0.5, 0.5}}}, []float32{1, 0}, 5, 10, 0.7)) != 1 {
		t.Fatal("0.7071 应 ≥ 0.70")
	}
	if len(longterm.RankSemantic([]longterm.Entry{{ID: 1, Content: "x", Embedding: []float32{0.5, 0.5}}}, []float32{1, 0}, 5, 10, 0.71)) != 0 {
		t.Fatal("0.7071 应 < 0.71")
	}
}

func ids(es []longterm.Entry) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}
