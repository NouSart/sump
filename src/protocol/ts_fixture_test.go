// 线路夹具的**反向**锚定之二：Go 解码不得依赖键序。
//
// 另一份手写实现（frontend/src/server/protocol.ts）产出的键序与 Go struct 序不同
// （Go：v,id,trace,boss,type,from,to,payload；TS：v,id,type,from,to,trace,boss,payload），
// 跨语言因此做不了字节相等，正确做法是「各钉各的字节 + 互相解码对方的字节」：
//   - protocol/fixtures_test.go：Go 必须产出夹具的那些字节（自身编码锚定）；
//   - 本测试：Go 必须能解出**外部实现**产出的那些字节（解码器不得依赖键序）。
package protocol

import (
	"encoding/base64"
	"testing"
)

// tsSetSettingJump 由真实 TS 编码器（encodeEnvelope）一次性产出后固化，键序即 TS 序。
// 注：产出方那一侧的「本侧产出 == 这些字节」断言不在本测试范围内（前端整体待重写）；
// 本测试只钉解码方向——Go 必须接受外来键序。
const tsSetSettingJump = "h6F2AaJpZLowMUZJWFRTU0VUVElORzAwMDAwMDAwMDAwMKR0eXBlpGp1bXCkZnJvbahmcm9udGVuZKJ0b69zZXR0aW5ncy1jZW50ZXKldHJhY2WoVFJBQ0UtVFOncGF5bG9hZIKmYWN0aW9uq3NldF9zZXR0aW5npWlucHV0g6dzZXJ2aWNlpXN2Yy1ho2tleaxkZW1vLnNldHRpbmeldmFsdWWjMTBt"

func TestGoDecodesTSFixture(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(tsSetSettingJump)
	if err != nil {
		t.Fatalf("TS 夹具不是合法 base64: %v", err)
	}
	env, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Go 无法解码 TS 产出的信封（前端 ↔ 中心的线路契约已漂移）: %v", err)
	}
	if env.V != ProtocolVersion || env.Type != TypeJump || env.From != "frontend" ||
		env.To != "settings-center" || env.Trace != "TRACE-TS" {
		t.Fatalf("信封字段不符: %+v", env)
	}
	var jp JumpPayload
	if err := env.DecodePayload(&jp); err != nil {
		t.Fatalf("解码 jump 载荷失败: %v", err)
	}
	if jp.Action != "set_setting" {
		t.Fatalf("action = %q, want set_setting", jp.Action)
	}
	var p SetSettingPayload
	if err := DecodeRaw(jp.Input, &p); err != nil {
		t.Fatalf("解码 set_setting 载荷失败: %v", err)
	}
	if p.Service != "svc-a" || p.Key != "demo.setting" || p.Value != "10m" {
		t.Fatalf("set_setting 载荷不符: %+v", p)
	}
}
