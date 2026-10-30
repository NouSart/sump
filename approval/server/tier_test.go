package server

import (
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
)

// newTierServer 造一个最小可用的审批服务（不启动传输层）。
func newTierServer(t *testing.T) *Server {
	t.Helper()
	e, err := engine.New(analysis.PowerShell{Executable: filepath.Join(t.TempDir(), "pwsh.exe")}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Engine: e, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDefaultTierIsLow：服务起来就是「低」档——只读命令不必再点，
// 即便设置中心还没下发过这一项。
func TestDefaultTierIsLow(t *testing.T) {
	s := newTierServer(t)
	if got := s.engine.Tier(); got != engine.TierLow {
		t.Fatalf("默认档位应为 low，实际 %s", got)
	}
}

// TestTierSettingIsDeclared：设置面板必须能看到这个项——中文名、说明、三个选项。
func TestTierSettingIsDeclared(t *testing.T) {
	s := newTierServer(t)
	var found *protocol.Setting
	for _, d := range s.Settings() {
		if d.Key == "auto_approve" {
			item := d
			found = &item
			break
		}
	}
	if found == nil {
		t.Fatal("设置项 auto_approve 没有声明")
	}
	if found.Kind != "enum" {
		t.Fatalf("应是选项控件，实际 %q", found.Kind)
	}
	if len(found.Options) != 4 || found.Options[0] != "off" || found.Options[1] != "low" ||
		found.Options[2] != "medium" || found.Options[3] != "workspace" {
		t.Fatalf("选项应为 off/low/medium/workspace，实际 %v", found.Options)
	}
	if found.Default != "low" {
		t.Fatalf("声明默认值应为 low，实际 %q", found.Default)
	}
	if found.Label == "" || found.Hint == "" {
		t.Fatal("缺少中文名或说明")
	}
}

// TestTierWorkspaceIsSelectable：第四档「工作区」必须能被面板选中并立即生效。
func TestTierWorkspaceIsSelectable(t *testing.T) {
	s := newTierServer(t)
	if err := s.ApplySetting("auto_approve", "workspace"); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.Tier(); got != engine.TierWorkspace {
		t.Fatalf("改档位后应为 workspace，实际 %s", got)
	}
	// 切回低档后，工作区档不再生效。
	if err := s.ApplySetting("auto_approve", "low"); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.Tier(); got != engine.TierLow {
		t.Fatalf("切回后应为 low，实际 %s", got)
	}
}

// TestConfigureChangesTier：面板改档位后立即生效（不等重启）。
func TestConfigureChangesTier(t *testing.T) {
	s := newTierServer(t)
	if err := s.ApplySetting("auto_approve", "medium"); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.Tier(); got != engine.TierMedium {
		t.Fatalf("改档位后应为 medium，实际 %s", got)
	}
	if err := s.ApplySetting("auto_approve", "off"); err != nil {
		t.Fatal(err)
	}
	if got := s.engine.Tier(); got != engine.TierOff {
		t.Fatalf("改回 off 后应为 off，实际 %s", got)
	}
	// 非法档位必须被拒绝，且保持原值。
	if err := s.ApplySetting("auto_approve", "mid"); err == nil {
		t.Fatal("非法档位应被拒绝")
	}
	if got := s.engine.Tier(); got != engine.TierOff {
		t.Fatalf("拒绝后应保持 off，实际 %s", got)
	}
}
