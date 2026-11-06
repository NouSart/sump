package shared

// 红线回归：共享库里不得出现原文 / 路径 / 链接 / 密钥形状 / 账号。
//
// 这组测试是给 2026-11-01 那次实际泄露写的——data/experience.db 里曾有
// 12 行 memories + 117 行 mem_entries，正文含绝对路径与 ws:// 地址加 token，
// conversation_id 含真实账号；逐字节比对确认它们全部来自旧库迁移
// （created_at 都早于 shared_meta 里的迁移时间）。
// 当时"泄露在形状上不可能发生"这句注释就写在包顶上。
//
// 下面所有路径 / 账号 / 令牌都是**造的**形状，不是任何真实值。
// 每条测试同时问两个问题：**该掩的掩了吗**、**该拒的拒了吗**；
// 摘掉对应写入路上的守卫，这里必须变红。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/memnet"
	"github.com/NouSart/sump/memory/redact"
	"github.com/NouSart/sump/memory/store"
)

// mustOpaque 把会话 id 收成库里实际存放的形状（测试拿它比对键）。
func mustOpaque(t *testing.T, raw string) string {
	t.Helper()
	v, err := redact.SessionID(raw)
	if err != nil {
		t.Fatalf("SessionID(%q) 出错: %v", raw, err)
	}
	return v
}

// 造的样本值：形状与事故里那几行同类，值本身不对应任何真实账号 / 机器 / 密钥。
const (
	fakeQQ        = "987654321"
	fakeWinPath   = `D:\srv\app\data\x.db`
	fakeWinFwd    = `C:/Windows/x` // 库里 rowid 80 的原句形状：open('C:/Windows/x','w')
	fakePosix     = `/srv/app/logs/app.log`
	fakeURL       = `ws://127.0.0.1:3000`
	fakeToken     = `sk-abcdefgh12345678`
	fakeTokenBare = `a1b2c3d4e5f6g7` // 14 位字母数字混合、**无前缀**：reToken 认不出，只能靠上下文
	fakePhone     = "13812345678"
	fakeEmail     = "someone@example.com"
	fakeTaskID    = "t20261101T170150-de9fa74c9a53"
	fakeCIDPlain  = "qq:private:" + fakeQQ
)

// leaky 是"绝不能进共享库"的样本值清单：断言直接盯原值，不盯形状，
// 因为掩码后留下的 "[路径]" 这类标记本身就该在场。
var leaky = []string{fakeQQ, fakeWinPath, fakeWinFwd, fakePosix, fakeURL, fakeToken, fakeTokenBare, fakePhone, fakeEmail}

// assertNoLeak 断言一批文本里不含任何一个样本原值。
func assertNoLeak(t *testing.T, what string, texts ...string) {
	t.Helper()
	joined := strings.Join(texts, "\n")
	for _, bad := range leaky {
		if strings.Contains(joined, bad) {
			t.Errorf("%s 里泄露了 %q", what, bad)
		}
	}
}

func TestRedlinePublishMasksBodyAndHashesAccount(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	published, err := s.Publish(now, PublishInput{
		SessionID:    fakeCIDPlain,
		SessionTitle: "运维会话，联系 " + fakePhone,
		TaskID:       fakeTaskID,
		TaskTitle:    `排查 ` + fakeWinPath,
		Goal:         `排查 ` + fakeWinPath + ` 的 NapCat 连接，默认地址 ` + fakeURL + `，令牌 ` + fakeToken,
		Outcome:      "日志见 " + fakePosix + "，联系 " + fakePhone + "；备用令牌 " + fakeTokenBare,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("有内容就该占目录位置")
	}

	// 两个 title 列必须查、而且必须有料：以前输入是空串，SELECT 到了也白到了，
	// 把 sessions.go 里的 Sanitize 摘掉测试一声不响。
	var sessionID, entry, sessionTitle string
	if err := s.db.QueryRow(`SELECT id, entry, title FROM shared_sessions LIMIT 1`).Scan(&sessionID, &entry, &sessionTitle); err != nil {
		t.Fatal(err)
	}
	var taskID, taskTitle, goal, outcome string
	if err := s.db.QueryRow(`SELECT task_id, title, goal, outcome FROM shared_tasks LIMIT 1`).Scan(&taskID, &taskTitle, &goal, &outcome); err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "shared_sessions", sessionID, entry, sessionTitle)
	assertNoLeak(t, "shared_tasks", taskID, taskTitle, goal, outcome)

	// 收口不是抹掉：分组键与入口都要留下，否则目录散架、"排除自己"也失效。
	if want := mustOpaque(t, fakeCIDPlain); sessionID != want {
		t.Errorf("会话键 = %q，期望收口形状 %q", sessionID, want)
	}
	if entry != "qq" {
		t.Errorf("入口列 = %q，期望 qq（入口是服务名，不是账号，应当留着）", entry)
	}
	if taskID != fakeTaskID {
		t.Errorf("task_id = %q，不透明 id 应原样保留", taskID)
	}
	if !strings.Contains(goal, "[路径]") || !strings.Contains(goal, "[链接]") || !strings.Contains(goal, "[已隐去]") {
		t.Errorf("正文该被掩码，实际 = %q", goal)
	}
	// 无前缀的裸令牌：reToken 够不着，只能靠"令牌 <值>"这一层上下文。
	if !strings.Contains(outcome, "[已隐去]") {
		t.Errorf("outcome 里的裸令牌该被上下文规则遮掉，实际 = %q", outcome)
	}
	if !strings.Contains(taskTitle, "[路径]") {
		t.Errorf("shared_tasks.title 该走同一道写入边界，实际 = %q", taskTitle)
	}
	if !strings.Contains(sessionTitle, "[已隐去]") {
		t.Errorf("shared_sessions.title 该走同一道写入边界，实际 = %q", sessionTitle)
	}
}

func TestRedlinePublishRejectsSmuggledIdentifiers(t *testing.T) {
	s := openTest(t)
	cases := []struct {
		name    string
		in      PublishInput
		whyWant string
	}{
		{"会话 id 塞路径", PublishInput{SessionID: fakeWinPath, TaskID: "t1", Goal: "g"}, "路径"},
		{"会话 id 塞邮箱", PublishInput{SessionID: fakeEmail, TaskID: "t1", Goal: "g"}, "邮箱"},
		{"task_id 塞账号", PublishInput{SessionID: "web:a", TaskID: fakeCIDPlain, Goal: "g"}, "冒号"},
		{"task_id 塞路径", PublishInput{SessionID: "web:a", TaskID: fakePosix, Goal: "g"}, "斜杠"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Publish(time.Now(), c.in); err == nil {
				t.Fatalf("不合规标识必须拒绝写入（期望错误里含 %q），实际成功", c.whyWant)
			} else if !strings.Contains(err.Error(), c.whyWant) {
				t.Errorf("错误信息该指明原因（含 %q），实际 = %v", c.whyWant, err)
			}
		})
	}
	// 拒绝 = 一行都不落：不能"这条写不进去就先写别的"。
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM shared_sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("被拒绝的发布留下了 %d 行", rows)
	}
}

func TestRedlineRememberPathMasksContent(t *testing.T) {
	s := openTest(t)
	// remember（Tool:true，模型可直接调）落在这里：Add 是它唯一的写入路。
	id, created, err := s.Longterm.Add("偏好", `在 `+fakeWinPath+` 用 PowerShell；密钥 `+fakeToken+`；轮换令牌 `+fakeTokenBare, 1, fakeCIDPlain, "reasoner")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("首次写入应新建")
	}
	entries, err := s.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != id {
		t.Fatalf("条目 = %+v", entries)
	}
	assertNoLeak(t, "memories", entries[0].Content, entries[0].ConversationID, entries[0].Kind, entries[0].EmbeddingModel)
	if !strings.Contains(entries[0].Content, "[路径]") || !strings.Contains(entries[0].Content, "[已隐去]") {
		t.Errorf("正文该被掩码，实际 = %q", entries[0].Content)
	}
	if want := mustOpaque(t, fakeCIDPlain); entries[0].ConversationID != want {
		t.Errorf("conversation_id = %q，期望 %q", entries[0].ConversationID, want)
	}
	if _, _, err := s.Longterm.Add("偏好", "别的内容", 1, "web:a", fakeWinPath); err == nil {
		t.Error("source 塞路径必须拒绝")
	}
}

func TestRedlineNotePathMasksContent(t *testing.T) {
	s := openTest(t)
	seedNetDirections(t, s.Net)
	res, err := s.Net.Attach(time.Now(), memnet.AttachInput{
		Content: `NapCat 默认地址 ` + fakeURL + `，令牌 ` + fakeTokenBare +
			`；脚本里 open('` + fakeWinFwd + `','w') 这样写`,
		ConversationID: fakeCIDPlain,
		TaskID:         "t1",
		Source:         "reasoner",
	}, []float32{1, 0, 0, 0, 0}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.EntryID == "" {
		t.Fatal("没写进去")
	}
	var content, cid, tid, source string
	if err := s.DB().QueryRow(`SELECT content, conversation_id, task_id, source FROM mem_entries WHERE id=?`, res.EntryID).
		Scan(&content, &cid, &tid, &source); err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "mem_entries", content, cid, tid, source)
	if !strings.Contains(content, "[链接]") || !strings.Contains(content, "[已隐去]") ||
		!strings.Contains(content, "[路径]") {
		t.Errorf("正文该被掩码，实际 = %q", content)
	}
	if want := mustOpaque(t, fakeCIDPlain); cid != want {
		t.Errorf("conversation_id = %q，期望 %q", cid, want)
	}
	if _, err := s.Net.Attach(time.Now(), memnet.AttachInput{
		Content: "别的内容", ConversationID: "web:a", TaskID: fakeWinPath, Source: "reasoner",
	}, []float32{0, 1, 0, 0, 0}, "test"); err == nil {
		t.Error("task_id 塞路径必须拒绝")
	}
	if _, err := s.Net.Attach(time.Now(), memnet.AttachInput{
		Content: "别的内容", ConversationID: fakePosix, Source: "reasoner",
	}, []float32{0, 1, 0, 0, 0}, "test"); err == nil {
		t.Error("conversation_id 塞路径必须拒绝")
	}
}

// TestRedlineMigrationMasksLegacyRows 钉住这次事故的那条路：
// 旧库逐行 INSERT 绕过 Add / Attach，历史上就是它把 129 行原样搬进共享库的。
func TestRedlineMigrationMasksLegacyRows(t *testing.T) {
	dir := t.TempDir()
	priv, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = priv.Close() })
	// 只为了在旧库里把表建出来——下面故意用裸 INSERT，模拟"守卫之前写下的历史数据"。
	if _, err := longterm.Open(priv.DB()); err != nil {
		t.Fatal(err)
	}
	if _, err := memnet.Open(priv.DB(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := priv.DB().Exec(
		`INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		 VALUES('事实', ?, 1, 'reasoner', ?, 'active', '2026-10-19T20:00:00Z', '2026-10-19T20:00:00Z')`,
		`把 QQ 的 NapCat 默认连接地址改成 `+fakeURL+`，并把 NapCat 令牌 `+fakeTokenBare+` 写成默认，`+fakeWinPath+` 的 qq/main.go 里有 defaultNapCatToken`,
		fakeCIDPlain,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := priv.DB().Exec(
		`INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
		   last_hit_at, created_at, embedding, embedding_model)
		 VALUES('e-leak', ?, ?, ?, 'reasoner', 'durable', 1, 'active', '2026-10-19T20:00:00Z', '2026-10-19T20:00:00Z', ?, 'fake')`,
		`工作目录是 `+fakeWinPath+`，仿真终端下显示为 `+fakePosix+
			`；脚本里 open('`+fakeWinFwd+`','w') 这样写`, fakeCIDPlain, fakeTaskID,
		[]byte{0, 0, 128, 63, 0, 0, 128, 63},
	); err != nil {
		t.Fatal(err)
	}
	// 一行标识列根本不合规的旧数据：正文要保住，分组键可以丢。
	if _, err := priv.DB().Exec(
		`INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
		   last_hit_at, created_at, embedding, embedding_model)
		 VALUES('e-badcid', '一条没有账号的历史经验', ?, '', 'agent', 'durable', 1, 'active',
		   '2026-10-19T20:00:00Z', '2026-10-19T20:00:00Z', ?, 'fake')`,
		fakeWinPath, []byte{0, 0, 128, 63, 0, 0, 128, 63},
	); err != nil {
		t.Fatal(err)
	}

	sh, err := Open(filepath.Join(dir, "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	migrated, err := sh.MigrateFrom(priv.DB())
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !migrated {
		t.Fatal("首次调用应真的搬了")
	}

	var memContent, memCID, memSource string
	if err := sh.DB().QueryRow(`SELECT content, COALESCE(conversation_id,''), source FROM memories LIMIT 1`).
		Scan(&memContent, &memCID, &memSource); err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "迁移后的 memories", memContent, memCID, memSource)
	if !strings.Contains(memContent, "[链接]") || !strings.Contains(memContent, "[已隐去]") {
		t.Errorf("迁移也该掩码正文，实际 = %q", memContent)
	}

	var noteContent, noteCID, noteTask string
	if err := sh.DB().QueryRow(`SELECT content, conversation_id, task_id FROM mem_entries WHERE id='e-leak'`).
		Scan(&noteContent, &noteCID, &noteTask); err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "迁移后的 mem_entries", noteContent, noteCID, noteTask)
	if noteTask != fakeTaskID {
		t.Errorf("合规的 task_id 该原样保留，实际 = %q", noteTask)
	}

	// 派生数据（向量字节）与经验本身都不许在迁移中丢失。
	var blob []byte
	if err := sh.DB().QueryRow(`SELECT embedding FROM mem_entries WHERE id='e-leak'`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if len(blob) != 8 {
		t.Errorf("向量字节 = %d，期望原样搬过来（8）", len(blob))
	}
	var badCID, badContent string
	if err := sh.DB().QueryRow(`SELECT conversation_id, content FROM mem_entries WHERE id='e-badcid'`).
		Scan(&badCID, &badContent); err != nil {
		t.Fatal(err)
	}
	if badCID != "" {
		t.Errorf("不合规的历史标识该置空，实际 = %q", badCID)
	}
	if !strings.Contains(badContent, "没有账号的历史经验") {
		t.Errorf("历史经验本身不该因为标识列不合规就被丢掉，实际 = %q", badContent)
	}
}

func TestRedlineForgetAcceptsBothShapes(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	if _, err := s.Publish(now, PublishInput{SessionID: fakeCIDPlain, TaskID: "t1", Goal: "一件事"}); err != nil {
		t.Fatal(err)
	}
	// 门户拿到的是收口后的键，复盘/脚本手里的是原值——两者都要删得到。
	opaque := mustOpaque(t, fakeCIDPlain)
	if res, err := s.Forget(opaque, ""); err != nil || res.Sessions != 1 {
		t.Fatalf("按收口形状删 = %+v, %v", res, err)
	}
	if _, err := s.Publish(now, PublishInput{SessionID: "web:a", TaskID: "t2", Goal: "另一件事"}); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Forget("web:a", ""); err != nil || res.Sessions != 1 {
		t.Fatalf("按原值形状删 = %+v, %v", res, err)
	}
}

func TestRedlineListExcludesSelfByOpaqueKey(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	for _, in := range []PublishInput{
		{SessionID: fakeCIDPlain, TaskID: "t1", Goal: "我的事"},
		{SessionID: "web:private:owner", TaskID: "t2", Goal: "别人的事"},
	} {
		if _, err := s.Publish(now, in); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(5, fakeCIDPlain, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != mustOpaque(t, "web:private:owner") {
		t.Fatalf("排除自己失效 = %+v", list)
	}
	var got string
	if err := s.db.QueryRow(`SELECT id FROM shared_sessions WHERE id = ?`, fakeCIDPlain).Scan(&got); err == nil {
		t.Errorf("库里出现了原值账号键: %q", got)
	}
}

// seedNetDirections 给五个预制大类钉上方向：Attach 要求挂靠，没方向时它拒绝写半条经验。
func seedNetDirections(t *testing.T, st *memnet.Store) {
	t.Helper()
	for i, id := range []string{"seed-pref", "seed-fact", "seed-how", "seed-pitfall", "seed-tradeoff"} {
		v := make([]float32, 5)
		v[i] = 1
		if err := st.SetNodeCentroid(id, v, "test"); err != nil {
			t.Fatal(err)
		}
	}
}
