package service

// 上报退避（防刷）单元测试：纯逻辑 + 假时钟。

import (
	"testing"
	"time"
)

// TestReportThrottle 验证放行序列：立即 → +1s → +2s → +4s…（逐档翻倍、封顶 30s）；静默 60s 后重置。
func TestReportThrottle(t *testing.T) {
	var th reportThrottle
	t0 := time.Unix(1000, 0)

	cases := []struct {
		at   time.Duration
		want bool
	}{
		{0, true},                       // 首次立即放行（下一档 1s）
		{500 * time.Millisecond, false}, // 1s 档内节流
		{1100 * time.Millisecond, true}, // +1s 放行（下一档 2s）
		{1500 * time.Millisecond, false},
		{3200 * time.Millisecond, true}, // +2s 放行（下一档 4s）
		{5000 * time.Millisecond, false},
		{7300 * time.Millisecond, true}, // +4s 放行（下一档 8s）
	}
	for i, tc := range cases {
		if got := th.allow(t0.Add(tc.at)); got != tc.want {
			t.Fatalf("case %d (t+%v): got %v, want %v", i, tc.at, got, tc.want)
		}
	}

	// 持续压力：放行间隔逐档翻倍，最终封顶 30s。
	last := time.Duration(0)
	maxSeen := time.Duration(0)
	sends := 0
	for i := 0; i <= 200; i++ { // 从 t+8s 起、每 1s 试一次，共 200s
		at := 8*time.Second + time.Duration(i)*time.Second
		if th.allow(t0.Add(at)) {
			if sends > 0 {
				if iv := at - last; iv > maxSeen {
					maxSeen = iv
				}
			}
			last = at
			sends++
		}
	}
	if maxSeen != reportMaxGap {
		t.Fatalf("放行间隔应封顶 %v，实测最大 %v", reportMaxGap, maxSeen)
	}

	// 静默重置：静默超过 60s 后回到 1s 档。
	quietAt := last + reportQuiet + time.Second
	if !th.allow(t0.Add(quietAt)) {
		t.Fatal("静默 60s 后应放行")
	}
	if th.allow(t0.Add(quietAt + 500*time.Millisecond)) {
		t.Fatal("重置后 1s 档内应节流")
	}
}
