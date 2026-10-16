// 线路夹具的**反向**锚定之一：Go 编码必须仍然产出这批字节。
//
// 这批固化字节（由 Go 一次性生成后写死）此前只被消费方用来「解码」——没有任何测试
// 约束「Go 仍然产出这些字节」。实测：把 Go 的 msgpack 标签 heartbeat_ms 改成 hb_ms，
// protocol 模块测试、embedding 的 pytest、根目录跨语言 e2e 三层全绿，
// 而线上的 Python / TS 消费方会静默读到零值。
//
// 对称的另一半在消费方侧：embedding/tests/test_protocol.py 用同样的输入重编码比对，
// 保证 Python 也不能静默改键名。
package protocol

import (
	"encoding/base64"
	"testing"
)

const (
	fixtureRegister  = "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVnaXN0ZXKkZnJvbaVzdmMtYaJ0b69zZXR0aW5ncy1jZW50ZXKncGF5bG9hZIakbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhqHByb3ZpZGVzkYOmYWN0aW9upHBpbmemb3V0cHV0pHBvbmekdG9vbMOoc2V0dGluZ3ORgqNrZXmsZGVtby5zZXR0aW5np2RlZmF1bHSiNW2saGVhcnRiZWF0X21z0wAAAAAAADqY"
	fixtureRoster    = "h6F2AaJpZLowMUZJWFJPU1RFUjAwMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0ypHR5cGWmcm9zdGVypGZyb22vc2V0dGluZ3MtY2VudGVyonRvpXN2Yy1hp3BheWxvYWSCqHNlcnZpY2VzkYSkbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhrGhlYXJ0YmVhdF9tc9MAAAAAAAA6mKhyZXZpc2lvbtMAAAAAAAAAAw=="
	fixtureResponse  = "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVzcG9uc2WkZnJvba9zZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIKib2vDpGRhdGGBqHNlcnZpY2VzkYKnc2VydmljZaVzdmMtYahzZXR0aW5nc5GFo2tleaxkZW1vLnNldHRpbmeldmFsdWWjMTBtp2RlZmF1bHSiNW2qb3ZlcnJpZGRlbsOmYWN0dWFsoA=="
	fixtureHeartbeat = "h6F2AaJpZLowMUZJWEhFQVJUQkVBVDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0zpHR5cGWpaGVhcnRiZWF0pGZyb22lc3ZjLWGidG+vc2V0dGluZ3MtY2VudGVyp3BheWxvYWSBpnN0YXR1c6Jvaw=="
	fixtureSettings  = "h6F2AaJpZLowMUZJWFNFVFRJTkdTMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS00pHR5cGWoc2V0dGluZ3OkZnJvba9zZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIOlY2F1c2Wrc2V0X3NldHRpbmenc2VydmljZaVzdmMtYadyZXN1bHRzkoOja2V5sHRvb2wubm90aWZ5X21vZGWldmFsdWWlYmF0Y2iib2vDg6NrZXmlZ2hvc3Sib2vCpWVycm9y2TLmnKrpgIHovr46IOacjeWKoeS4jeWcqOWQjeWGjO+8iOWPr+iDveW3suemu+e6v++8iQ=="
)

// fixtureEnvelope 用固定 id / trace 构造信封（payload 走真实编码路径）。
func fixtureEnvelope(t *testing.T, id, trace, typ, from, to string, payload any) Envelope {
	t.Helper()
	raw, err := EncodePayload(payload)
	if err != nil {
		t.Fatalf("编码 payload 失败: %v", err)
	}
	return Envelope{V: ProtocolVersion, ID: id, Trace: trace, Type: typ, From: from, To: to, Payload: raw}
}

func TestGoEncodesFrozenFixtures(t *testing.T) {
	settingsData, err := EncodePayload(SettingsResult{Services: []ServiceSettings{{
		Service:  "svc-a",
		Settings: []SettingView{{Key: "demo.setting", Value: "10m", Default: "5m", Overridden: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		env     Envelope
		wantB64 string
	}{
		{"register", fixtureEnvelope(t, "01FIXREGISTER0000000000000", "TRACE-1", TypeRegister, "svc-a", "settings-center", RegisterPayload{
			Name:        "svc-a",
			Addr:        "ws://127.0.0.1:9999/ws",
			Description: "夹具服务",
			Provides:    []Provide{{Action: "ping", Output: "pong", Tool: true}},
			Settings:    []Setting{{Key: "demo.setting", Default: "5m"}},
			HeartbeatMS: 15000,
		}), fixtureRegister},
		{"roster", fixtureEnvelope(t, "01FIXROSTER000000000000000", "TRACE-2", TypeRoster, "settings-center", "svc-a", RosterPayload{
			Services: []ServiceCard{{Name: "svc-a", Addr: "ws://127.0.0.1:9999/ws", Description: "夹具服务", HeartbeatMS: 15000}},
			Revision: 3,
		}), fixtureRoster},
		{"response", fixtureEnvelope(t, "01FIXREGISTER0000000000000", "TRACE-1", TypeResponse, "settings-center", "svc-a", ResponsePayload{
			OK: true, Data: settingsData,
		}), fixtureResponse},
		{"heartbeat", fixtureEnvelope(t, "01FIXHEARTBEAT000000000000", "TRACE-3", TypeHeartbeat, "svc-a", "settings-center", HeartbeatPayload{Status: "ok"}), fixtureHeartbeat},
		{"settings", fixtureEnvelope(t, "01FIXSETTINGS0000000000000", "TRACE-4", TypeSettings, "settings-center", "svc-a", SettingsEventPayload{
			Cause: "set_setting", Service: "svc-a",
			Results: []SettingResult{
				{Key: "tool.notify_mode", Value: "batch", OK: true},
				{Key: "ghost", OK: false, Error: "未送达: 服务不在名册（可能已离线）"},
			},
		}), fixtureSettings},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := Marshal(tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if got := base64.StdEncoding.EncodeToString(data); got != tc.wantB64 {
				t.Fatalf("Go 编码与固化夹具不一致（线路契约已漂移）：\n got=%s\nwant=%s", got, tc.wantB64)
			}
		})
	}
}
