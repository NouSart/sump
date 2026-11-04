package server

// capabilities 是管理/调试动作：面板与运维靠它判断方言覆盖、第二层是否开启、当前档位。
// 这里直接调处理器（不启传输层），把返回字段逐项钉住。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
)

func TestCapabilitiesReportsDialectsAndReviewState(t *testing.T) {
	s := newTierServer(t)

	out, err := s.capabilities(context.Background(), protocol.Envelope{}, protocol.JumpPayload{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("capabilities 应返回对象，实际 %T", out)
	}
	if got["schema_version"] != 1 {
		t.Fatalf("schema_version 应为 1: %+v", got)
	}
	dialects, _ := got["dialects"].([]string)
	if len(dialects) != 2 || dialects[0] != "powershell" || dialects[1] != "bash" {
		t.Fatalf("dialects 应列出能自动分析的 powershell 与 bash: %+v", got["dialects"])
	}
	if got["profile"] != protocol.PowerShellProfile || got["other_dialects"] != "review" {
		t.Fatalf("方言兜底口径不符: %+v", got)
	}
	if got["feedback_route"] != "shell.report_result" {
		t.Fatalf("feedback_route 不符: %+v", got)
	}
	if got["review_enabled"] != false || got["review_portal"] != "" {
		t.Fatalf("未配置门户时第二层应关闭: %+v", got)
	}
	if got["auto_approve"] != "low" {
		t.Fatalf("默认档位应为 low: %+v", got)
	}
	if got["pending"] != 0 || got["accepted"] != uint64(0) || got["busy"] != uint64(0) {
		t.Fatalf("初始计数应全零: %+v", got)
	}
	if ttl, _ := got["review_ttl_ms"].(int64); ttl <= 0 {
		t.Fatalf("review_ttl_ms 应为正数: %+v", got)
	}

	// 配了门户：enabled 打开、portal 名字与 TTL 原样上报。
	e, err := engine.New(analysis.PowerShell{Executable: filepath.Join(t.TempDir(), "pwsh.exe")}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	withPortal, err := New(Config{
		Engine: e, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0),
		Portal: "frontend", ReviewTTL: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = withPortal.capabilities(context.Background(), protocol.Envelope{}, protocol.JumpPayload{})
	if err != nil {
		t.Fatal(err)
	}
	got = out.(map[string]any)
	if got["review_enabled"] != true || got["review_portal"] != "frontend" {
		t.Fatalf("第二层状态不符: %+v", got)
	}
	if got["review_ttl_ms"] != int64(3000) {
		t.Fatalf("review_ttl_ms 应取配置值: %+v", got)
	}
}
