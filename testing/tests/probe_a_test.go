// 探针 A —— 头部格式：注释开头（历史高危格式）。
// 用途：监控是否有工具/扩展在文件顶部插入 package 行（2026-09-26 事件调查）。
// 若本文件出现第二个 package 声明，说明该"插入"机制仍活跃，且与头部格式有关。
package tests

import "testing"

// TestProbeA 空测试：保持文件参与编译（被插入 package 时会编译失败，即为报警）。
func TestProbeA(t *testing.T) {}
