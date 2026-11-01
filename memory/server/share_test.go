package server

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/shared"
)

func TestJoinDigest(t *testing.T) {
	cases := [][3]string{
		{"", "", ""},
		{"在改配色", "", "在改配色"},
		{"在改配色", "在改配色", "在改配色"},
		{"在改配色", "还没定稿", "在改配色；还没定稿"},
		{"在改配色，还没定稿", "还没定稿", "在改配色，还没定稿"},
	}
	for _, c := range cases {
		if got := joinDigest(c[0], c[1]); got != c[2] {
			t.Fatalf("joinDigest(%q,%q) = %q，期望 %q", c[0], c[1], got, c[2])
		}
	}
}

func TestAgoText(t *testing.T) {
	now := time.Now()
	cases := map[time.Duration]string{
		30 * time.Second:    "刚刚",
		3 * time.Minute:     "3 分钟前",
		5 * time.Hour:       "5 小时前",
		3 * 24 * time.Hour:  "3 天前",
		40 * 24 * time.Hour: now.Add(-40 * 24 * time.Hour).Format("2006-01-02"),
	}
	for d, want := range cases {
		if got := agoText(now.Add(-d), now); got != want {
			t.Fatalf("agoText(%v) = %q，期望 %q", d, got, want)
		}
	}
	if got := agoText(time.Time{}, now); got != "" {
		t.Fatalf("零值时间应为空: %q", got)
	}
}

func TestSessionLines(t *testing.T) {
	now := time.Now()
	sessions := []shared.Session{
		{
			ID: "web:a", Entry: "web", Title: "配色调整", UpdatedAt: now.Add(-2 * time.Hour),
			Tasks: []shared.Task{
				{Title: "改主题", Goal: "把主题改成跟随本地时间", Outcome: "还没定稿", Capabilities: []string{"shell", "files"}},
				{Title: "旧任务", Goal: "已经不需要了"},
			},
		},
		{ID: "qq:1", Entry: "qq", Title: "", UpdatedAt: now.Add(-72 * time.Hour), Tasks: []shared.Task{{Goal: "报错排查"}}},
		{ID: "web:empty", Entry: "web", Tasks: nil},
	}
	lines := sessionLines(sessions, 120)
	if len(lines) != 2 {
		t.Fatalf("没有摘要的会话不该出现: %v", lines)
	}
	if !strings.Contains(lines[0], "门户「配色调整」") || !strings.Contains(lines[0], "2 小时前") || !strings.Contains(lines[0], "shell/files") {
		t.Fatalf("第一行 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "QQ「未命名会话」：报错排查") || !strings.Contains(lines[1], "3 天前") {
		t.Fatalf("第二行 = %q", lines[1])
	}
}

func TestSessionLinesDigestBudget(t *testing.T) {
	long := strings.Repeat("很长的摘要", 50)
	lines := sessionLines([]shared.Session{{
		ID: "web:a", Entry: "web", Title: "x", UpdatedAt: time.Now(),
		Tasks: []shared.Task{{Goal: long}},
	}}, 20)
	if len(lines) != 1 {
		t.Fatalf("行数 = %d", len(lines))
	}
	if len([]rune(lines[0])) > 200 {
		t.Fatalf("摘要没有按预算截断: %d 字", len([]rune(lines[0])))
	}
	if !strings.Contains(lines[0], "…") {
		t.Fatalf("超长摘要应带省略号: %q", lines[0])
	}
}
