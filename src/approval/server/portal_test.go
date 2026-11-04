// 门户闸门的直接测试。
//
// 之前 `portalInRoster` 从未被执行过：三个用例都把 `portalReady` 换成桩，
// 于是「门户不在名册 → 不挂起、直接按 review 返回」这条 fail-closed 语义
// 改坏了也没人知道。这里补上对真实判据的断言。
package server

import (
	"context"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestPortalInRoster(t *testing.T) {
	roster := []protocol.ServiceCard{{Name: "frontend"}, {Name: "shell"}}

	for _, tc := range []struct {
		name   string
		portal string
		cards  []protocol.ServiceCard
		want   bool
	}{
		{name: "在名册里", portal: "frontend", cards: roster, want: true},
		{name: "不在名册里", portal: "ghost", cards: roster, want: false},
		{name: "门户未配置", portal: "", cards: roster, want: false},
		{name: "名册为空", portal: "frontend", cards: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := portalInRoster(tc.portal, tc.cards); got != tc.want {
				t.Fatalf("portalInRoster(%q, %d 张名片) = %v, want %v", tc.portal, len(tc.cards), got, tc.want)
			}
		})
	}
}

// portalReady 的真实现必须走 portalInRoster，而不是别的捷径。
func TestPortalReadyUsesRoster(t *testing.T) {
	s, _ := newReviewServer(t, func(c *Config) { c.Portal = "frontend" })
	// 名册里没有 frontend（New 之后没注册过任何服务）→ 无人裁决。
	s.portalReady = func() bool { return portalInRoster(s.portalNow(), s.Roster()) }
	if s.portalReady() {
		t.Fatal("名册里没有门户时 portalReady 必须为 false（fail-closed）")
	}
}

// approval_list 只给门户拉；别的服务（哪怕自称 shell）一律拒。
func TestApprovalListOnlyPortal(t *testing.T) {
	s, _ := newReviewServer(t, nil)

	out, err := s.list(context.Background(), protocol.Envelope{From: "frontend"}, protocol.JumpPayload{Action: protocol.ApprovalListAction})
	if err != nil {
		t.Fatalf("门户拉取被拒: %v", err)
	}
	if got, ok := out.(protocol.PendingApprovalList); !ok || len(got.Items) != 0 {
		t.Fatalf("空队列应返回空列表: %#v", out)
	}

	for _, from := range []string{"shell", "reasoner", "frontend-fake", ""} {
		if _, err := s.list(context.Background(), protocol.Envelope{From: from}, protocol.JumpPayload{Action: protocol.ApprovalListAction}); err == nil {
			t.Fatalf("from=%q 不该能拉待审项", from)
		} else if !strings.Contains(err.Error(), "只有门户") {
			t.Fatalf("from=%q 的错误应指明只有门户，实际: %v", from, err)
		}
	}
}

// 门户未配置时，approval_list 同样拒绝（不要把队列暴露给「谁都不是」的调用方）。
func TestApprovalListRejectedWithoutPortal(t *testing.T) {
	s, _ := newReviewServer(t, func(c *Config) { c.Portal = "" })
	if _, err := s.list(context.Background(), protocol.Envelope{From: ""}, protocol.JumpPayload{Action: protocol.ApprovalListAction}); err == nil {
		t.Fatal("未配置门户时不该能拉待审项")
	}
}
