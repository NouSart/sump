package tests

// qq.deliver 的失败路径：空文本、会话标识不合法、未知会话类型、用户 id 非法、NapCat 发送失败。
// 成功路径在 service_test.go / group_test.go；这些分支决定“链终点没送到”时调用方看到什么。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// deliverOnce 向 qq 发一次原生 deliver：成功即 OK，被拒则带回错误文本。
func deliverOnce(t *testing.T, qqURL, text, conversationID string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, qqURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call, err := spv2.NewCall("trace-deliver", spv2.AgentRef("reasoner").WithAuthority(spv2.AuthorityRuntime), spv2.ServiceRef("qq"), "deliver",
		protocol.DeliverPayload{Text: text, ConversationID: conversationID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err != nil {
		return outcome{Error: err.Error()}
	}
	return outcome{OK: true}
}

func TestDeliverRejectsBadInput(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	cases := []struct {
		name           string
		text           string
		conversationID string
		want           string
	}{
		{"empty_text", "  ", "qq:private:2271917353", "text 为空"},
		{"bad_conversation", "回复", "not-a-conversation", "无法解析会话标识"},
		{"unknown_kind", "回复", "qq:channel:123456", "未知会话类型"},
		{"bad_user_id", "回复", "qq:private:abc", "会话 id 无效"},
		{"bad_group_id", "回复", "qq:group:abc", "会话 id 无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := deliverOnce(t, qq.WsURL(), tc.text, tc.conversationID)
			if rp.OK || !strings.Contains(rp.Error, tc.want) {
				t.Fatalf("deliver 应被拒并说明 %q: ok=%v err=%q", tc.want, rp.OK, rp.Error)
			}
		})
	}
}

func TestDeliverReportsSendFailure(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.mu.Lock()
	nc.failSend = true
	nc.mu.Unlock()

	rp := deliverOnce(t, qq.WsURL(), "回复", "qq:private:2271917353")
	if rp.OK {
		t.Fatal("NapCat 发送失败时 deliver 不应报成功")
	}
	if !strings.Contains(rp.Error, "发送 QQ 消息失败") || !strings.Contains(rp.Error, "failed") {
		t.Fatalf("错误应带上底层原因: %q", rp.Error)
	}
}
