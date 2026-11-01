package napcat

// 事件解析：来源标注要靠这几个方法取事实（发送者怎么称呼、有没有 @ 我）。
// 它们不碰网络，纯解析——所以直接钉输入输出，不必起连接。

import (
	"encoding/json"
	"testing"
)

func event(t *testing.T, raw string) Event {
	t.Helper()
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("事件 JSON 解析失败: %v", err)
	}
	return ev
}

// TestDisplayName 群名片优先于昵称；都没有就是空串（不造假名字）。
func TestDisplayName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"card_first", `{"sender":{"nickname":"张三","card":"产品-张三"}}`, "产品-张三"},
		{"nickname_only", `{"sender":{"nickname":"张三","card":""}}`, "张三"},
		{"nothing", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := event(t, tc.raw).DisplayName(); got != tc.want {
				t.Fatalf("DisplayName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMentions 群里的 @ 判定：段数组（字符串 / 数字 qq）与 CQ 码字符串都要认。
func TestMentions(t *testing.T) {
	const self = int64(2578428650)
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"segment_string", `{"message":[{"type":"at","data":{"qq":"2578428650"}},{"type":"text","data":{"text":"在吗"}}]}`, true},
		{"segment_number", `{"message":[{"type":"at","data":{"qq":2578428650}}]}`, true},
		{"segment_other", `{"message":[{"type":"at","data":{"qq":"111"}}]}`, false},
		{"segment_at_all", `{"message":[{"type":"at","data":{"qq":"all"}}]}`, false},
		{"no_at", `{"message":[{"type":"text","data":{"text":"随便说"}}]}`, false},
		{"cq_string", `{"message":"[CQ:at,qq=2578428650] 在吗"}`, true},
		{"cq_string_other", `{"message":"[CQ:at,qq=111] 在吗"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := event(t, tc.raw).Mentions(self); got != tc.want {
				t.Fatalf("Mentions() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMentionsUnknownSelf 不知道自己在群里叫什么（self_id 缺失）时一律不算被点到——
// 猜"被点名了"会让机器人在没被叫的群里自言自语。
func TestMentionsUnknownSelf(t *testing.T) {
	ev := event(t, `{"message":[{"type":"at","data":{"qq":"all"}}]}`)
	if ev.Mentions(0) {
		t.Fatal("self_id 未知时不应声称被 @")
	}
}
