package shared

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通文本原样", "在改门户的时间主题", "在改门户的时间主题"},
		{"链接", "参考 https://example.com/a?b=1 的做法", "参考 [链接] 的做法"},
		{"Windows 路径", `打开 C:\work\sump\data\memory.db 看看`, "打开 [路径] 看看"},
		{"POSIX 路径", "日志在 /var/log/sump/app.log 里", "日志在 [路径] 里"},
		{"邮箱", "发到 someone@example.com", "发到 [邮箱]"},
		{"密钥", "key 是 sk-abcdefgh12345678 别外传", "key 是 [已隐去] 别外传"},
		{"Bearer", "header Bearer abc.def-ghi", "header [已隐去]"},
		{"手机号", "打 13812345678 找他", "打 [已隐去] 找他"},
		{"换行压平", "第一行\n第二行\r\n第三行", "第一行 第二行 第三行"},
		{"空串", "   \n\t ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Sanitize(c.in, 0); got != c.want {
				t.Fatalf("Sanitize(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
	if got := Sanitize("一二三四五六", 4); got != "一二三…" {
		t.Fatalf("按字符截断不对: %q", got)
	}
	if got := Sanitize("一二三四五", 5); got != "一二三四五" {
		t.Fatalf("刚好等于上限不该截断: %q", got)
	}
}

func TestSanitizeCapabilities(t *testing.T) {
	got := SanitizeCapabilities([]string{"shell", "Shell", " files ", "bad name", "a/b", "", "shell"})
	want := []string{"shell", "files"}
	if len(got) != len(want) {
		t.Fatalf("能力族 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("能力族 = %v，期望 %v", got, want)
		}
	}
	many := SanitizeCapabilities([]string{"a", "b", "c", "d", "e", "f", "g"})
	if len(many) != 6 {
		t.Fatalf("应封顶 6 个，得到 %d", len(many))
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPublishAndList(t *testing.T) {
	s := openTest(t)
	now := time.Now()

	ok, err := s.Publish(now, PublishInput{
		SessionID: "web:private:owner", SessionTitle: "门户主会话",
		TaskID: "t1", TaskTitle: "改时间主题",
		Goal: "把时间主题改成跟随本地时间", Outcome: "做到一半",
		Capabilities: []string{"shell", "files"},
	})
	if err != nil || !ok {
		t.Fatalf("Publish = %v, %v", ok, err)
	}

	list, err := s.List(5, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Entry != "web" || list[0].Title != "门户主会话" {
		t.Fatalf("目录 = %+v", list)
	}
	if len(list[0].Tasks) != 1 || list[0].Tasks[0].Goal == "" {
		t.Fatalf("任务 = %+v", list[0].Tasks)
	}

	// 自己排除：当前会话不该出现在自己的目录里。
	self, err := s.List(5, "web:private:owner", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(self) != 0 {
		t.Fatalf("排除自己失败: %+v", self)
	}
}

func TestPublishPrivateWritesNothing(t *testing.T) {
	s := openTest(t)
	ok, err := s.Publish(time.Now(), PublishInput{
		SessionID: "web:private:owner", TaskID: "t1", Goal: "机密内容", Private: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("private 任务不该写目录")
	}
	list, _ := s.List(5, "", "")
	if len(list) != 0 {
		t.Fatalf("private 任务写进去了: %+v", list)
	}
}

func TestPublishEmptyDigestSkipped(t *testing.T) {
	s := openTest(t)
	ok, err := s.Publish(time.Now(), PublishInput{SessionID: "web:x", TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("空摘要不该占目录位置")
	}
}

func TestPublishKeepsLatestTasks(t *testing.T) {
	s := openTest(t)
	base := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := s.Publish(base.Add(time.Duration(i)*time.Minute), PublishInput{
			SessionID: "qq:private:1", TaskID: "t" + string(rune('a'+i)), Goal: "第 " + string(rune('1'+i)) + " 件事",
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(5, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("会话数 = %d", len(list))
	}
	if len(list[0].Tasks) != MaxTasksPerSession {
		t.Fatalf("任务数 = %d，期望 %d", len(list[0].Tasks), MaxTasksPerSession)
	}
	if list[0].Tasks[0].Goal != "第 5 件事" {
		t.Fatalf("最新一条应在最前: %+v", list[0].Tasks)
	}

	// 库里的行数也要被裁到上限：List 自己就带 LIMIT，只看列表看不出"根本没删"。
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM shared_tasks WHERE session_id = ?`, "qq:private:1").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != MaxTasksPerSession {
		t.Fatalf("库里留下 %d 行，期望 %d（发布时就该裁掉更旧的）", rows, MaxTasksPerSession)
	}
}

func TestPublishRepublishIsOverwrite(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	in := PublishInput{SessionID: "web:a", TaskID: "t1", Goal: "第一版"}
	if _, err := s.Publish(now, in); err != nil {
		t.Fatal(err)
	}
	in.Goal = "第二版"
	if _, err := s.Publish(now.Add(time.Minute), in); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(5, "", "")
	if len(list) != 1 || len(list[0].Tasks) != 1 || list[0].Tasks[0].Goal != "第二版" {
		t.Fatalf("重复发布应覆盖: %+v", list)
	}
}

func TestPublishRequiresIDs(t *testing.T) {
	s := openTest(t)
	if _, err := s.Publish(time.Now(), PublishInput{TaskID: "t1"}); err == nil {
		t.Fatal("缺 conversation_id 应报错")
	}
	if _, err := s.Publish(time.Now(), PublishInput{SessionID: "web:a"}); err == nil {
		t.Fatal("缺 task_id 应报错")
	}
}

func TestListQueryFilter(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	if _, err := s.Publish(now, PublishInput{SessionID: "web:a", TaskID: "t1", Goal: "排查 embedding 缺模型"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(now, PublishInput{SessionID: "web:b", TaskID: "t2", Goal: "改门户配色"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(5, "", "embedding")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "web:a" {
		t.Fatalf("过滤结果 = %+v", list)
	}
}

func TestForget(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	for _, p := range []PublishInput{
		{SessionID: "web:a", TaskID: "t1", Goal: "一件事"},
		{SessionID: "web:a", TaskID: "t2", Goal: "另一件事"},
		{SessionID: "web:b", TaskID: "t3", Goal: "第三件事"},
	} {
		if _, err := s.Publish(now, p); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Forget("", "t1")
	if err != nil || res.Tasks != 1 || res.Sessions != 0 {
		t.Fatalf("删单条 = %+v, %v", res, err)
	}
	list, _ := s.List(5, "", "")
	if len(list) != 2 {
		t.Fatalf("删一条后会话数 = %d", len(list))
	}

	res, err = s.Forget("web:a", "")
	if err != nil || res.Tasks != 1 || res.Sessions != 1 {
		t.Fatalf("删整个会话 = %+v, %v", res, err)
	}

	// 删到空：会话行也不该留着。
	if _, err := s.Forget("", "t3"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(5, "", "")
	if len(list) != 0 {
		t.Fatalf("删空后目录应为空: %+v", list)
	}

	if _, err := s.Forget("", ""); err == nil {
		t.Fatal("两个 id 都不给应报错")
	}
	if res, err := s.Forget("", "missing"); err != nil || res.Tasks != 0 {
		t.Fatalf("删不存在的条目应是无操作: %+v, %v", res, err)
	}
}
