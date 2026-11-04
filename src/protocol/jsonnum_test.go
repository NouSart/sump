// JSON 数字归一化：LLM 工具参数的数字必须还原成整数，否则服务侧整数字段解码会失败。
package protocol

import "testing"

func TestNormalizeJSONNumbers(t *testing.T) {
	// 整数值的 float64 → int64（JSON 里 "0" / "8000" 解出来都是 float64）
	if got := NormalizeJSONNumbers(float64(8000)); got != any(int64(8000)) {
		t.Fatalf("整数应还原成 int64，得到 %T(%v)", got, got)
	}
	// 非整数保持浮点
	if got := NormalizeJSONNumbers(1.5); got != any(1.5) {
		t.Fatalf("非整数应保持 float64，得到 %T(%v)", got, got)
	}
	// 超出 int64 的整数保持浮点（宁可不转，也不能截断成错的整数）
	if got := NormalizeJSONNumbers(1e30); got != any(1e30) {
		t.Fatalf("超出范围应保持 float64，得到 %T(%v)", got, got)
	}
	// 递归：嵌套 map 与切片
	got := NormalizeJSONNumbers(map[string]any{
		"ms":     float64(300),
		"note":   "睡一会",
		"nested": map[string]any{"priority": float64(0)},
		"list":   []any{float64(1), 2.5},
	})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("顶层应仍是 map，得到 %T", got)
	}
	if m["ms"] != any(int64(300)) || m["nested"].(map[string]any)["priority"] != any(int64(0)) {
		t.Fatalf("嵌套数字未归一: %+v", m)
	}
	if list := m["list"].([]any); list[0] != any(int64(1)) || list[1] != any(2.5) {
		t.Fatalf("切片元素未按规则处理: %+v", list)
	}
	if m["note"] != any("睡一会") {
		t.Fatalf("非数字不该被动: %+v", m["note"])
	}
}

// TestNormalizeJSONNumbersFixesDecode 是当初线上故障的回归：
// 未归一时（float64）解码进整数字段直接失败；归一后解码正常。
func TestNormalizeJSONNumbersFixesDecode(t *testing.T) {
	raw, err := EncodePayload(map[string]any{"priority": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	var bad RememberPayload
	if err := DecodeRaw(raw, &bad); err == nil {
		t.Fatal("前提不成立：float64 竟然能解码进 int 字段")
	}

	raw, err = EncodePayload(NormalizeJSONNumbers(map[string]any{"priority": float64(3)}))
	if err != nil {
		t.Fatal(err)
	}
	var ok RememberPayload
	if err := DecodeRaw(raw, &ok); err != nil {
		t.Fatalf("归一后应能解码: %v", err)
	}
	if ok.Priority != 3 {
		t.Fatalf("priority 应为 3，得到 %d", ok.Priority)
	}
}
