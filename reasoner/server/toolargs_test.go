// 工具参数边界：LLM 给的 JSON 数字必须先归一成整数，否则服务侧整数字段解码会失败
// （实测故障：timer.sleep 的 ms、memory.remember 的 priority 都报 msgpack invalid code=cb）。
package server

import (
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

func TestArgsToRawNormalizesNumbers(t *testing.T) {
	raw, err := argsToRaw(`{"kind":"身份","content":"我叫测试者","priority":3}`)
	if err != nil {
		t.Fatal(err)
	}
	var p protocol.RememberPayload
	if err := protocol.DecodeRaw(raw, &p); err != nil {
		t.Fatalf("工具参数应能被协议载荷解码（整数字段）：%v", err)
	}
	if p.Priority != 3 || p.Kind != "身份" || p.Content != "我叫测试者" {
		t.Fatalf("载荷不符: %+v", p)
	}

	// 嵌套数字同样归一（例如 {ms: 8000} 这类）
	raw, err = argsToRaw(`{"ms":8000,"note":"测试","extra":{"priority":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := protocol.DecodeRaw(raw, &v); err != nil {
		t.Fatalf("嵌套结构应能解码: %v", err)
	}
	if v["ms"] != any(int64(8000)) {
		t.Fatalf("ms 应归一成 int64，得到 %T(%v)", v["ms"], v["ms"])
	}
}
