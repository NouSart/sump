package tests

// 探针 B —— 头部格式：package 首行（候选防御格式）。
// 用途：若本文件也出现"第二个 package"（或其他头部插入），
// 则说明"插入"机制与头部格式无关。2026-09-26 事件调查。

import "testing"

// TestProbeB 空测试：保持文件参与编译。
func TestProbeB(t *testing.T) {}
