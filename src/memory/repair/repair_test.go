package repair

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/redact"
	"github.com/NouSart/sump/memory/shared"
)

// 这些用例面对的是"存量"：红线立起来之前就已经躺在共享库里的行。
// 写入守卫（redact）管不到它们，只有这个清理工具能管。

// 真实泄露的形状（2026-11-01 那次事故里的样子）：正文带 UNC 路径与链接，
// 会话键里带 QQ 号。
//
// fakeQQ 必须是**编造的**号：这里的值刻意不是任何人的账号（全 1 的 10 位数），
// 因为测试文件会进 git，而"真实账号只活在 data/ 里、不进源码"本身就是这条红线的一部分。
// 换成真号，用例照样过，但那等于把事故里被清出去的东西重新写回仓库。
const (
	fakeQQ     = "1111111111"
	legacyCID  = "qq:private:" + fakeQQ
	legacyBody = `去 \\nas\share\dump.rdb 里取，备用地址 https://token.example/sk-abcdefghij012345`
)

// newLegacyDB 造一个"迁移过一次、守卫还没立起来"的共享库。
func newLegacyDB(t *testing.T) *shared.Store {
	t.Helper()
	sh, err := shared.Open(filepath.Join(t.TempDir(), "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	db := sh.DB()
	if _, err := db.Exec(
		`INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		 VALUES('偏好', ?, 1, 'reasoner', NULL, 'active', ?, ?)`, legacyBody, now(), now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(
			`INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
			   last_hit_at, created_at, embedding_model)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			entryID(i), "计数用文件落盘，重启不倒流 "+legacyBody, legacyCID,
			"weird/task id", "reasoner", "durable", 1, "active", now(), now(), "fake"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO mem_nodes(id, parent_id, level, anchor, entries, created_at, updated_at)
		 VALUES('n-legacy','',1,?,0,?,?)`, "样本：\\\\attacker.host\\c$\\secrets 里的配置", now(), now()); err != nil {
		t.Fatal(err)
	}
	return sh
}

func now() string { return time.Now().Format(time.RFC3339Nano) }

func entryID(i int) string { return fmt.Sprintf("e-legacy-%03d", i) }

// TestTablesMatchSharedSchema 锁住"表名列名还在"这件事。
// 清理是按名字拼 SQL 的：列改名后 UPDATE 会直接失败，而失败只有跑到真库上才看得见。
func TestTablesMatchSharedSchema(t *testing.T) {
	sh := newLegacyDB(t)
	for _, tb := range tables {
		for _, c := range tb.cols {
			var cnt int
			if err := sh.DB().QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, tb.name, c.name).Scan(&cnt); err != nil {
				t.Fatal(err)
			}
			if cnt != 1 {
				t.Errorf("tables 里声明了 %s.%s，但共享库里没有这一列——清理清单该跟着改", tb.name, c.name)
			}
		}
	}
}

// TestDryRunWritesNothing 是"预演"的定义：报告说有得洗，库里一个字节都没动。
func TestDryRunWritesNothing(t *testing.T) {
	sh := newLegacyDB(t)
	before := cellDump(t, sh)

	rep, err := Clean(sh.DB(), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || rep.Committed {
		t.Errorf("预演不该标记成写盘: %+v", rep)
	}
	if rep.Tables == nil {
		t.Fatal("报告里没有表统计")
	}
	total := 0
	for _, s := range rep.Tables {
		total += s.Changed
	}
	if total == 0 {
		t.Error("这份库明明有泄露，预演却报告零改写")
	}
	if got := cellDump(t, sh); got != before {
		t.Error("预演把库改了")
	}
}

// TestApplyScrubsAndRechecks 是清理的主路径：洗完复检必须归零。
func TestApplyScrubsAndRechecks(t *testing.T) {
	sh := newLegacyDB(t)

	if rep, err := Clean(sh.DB(), true); err != nil {
		t.Fatal(err)
	} else {
		if !rep.Applied || !rep.Committed {
			t.Errorf("apply=true 且复检干净时应已提交: %+v", rep)
		}
		if len(rep.Remaining) != 0 {
			t.Errorf("洗完复检仍命中: %v", rep.Remaining)
		}
	}

	// 复检独立再跑一次：不复用 Clean 里的那份结果。
	bad, err := Verify(sh.DB())
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("Verify 仍报命中: %v", bad)
	}

	// 正文：账号 / 链接 / 路径形状都得没了，但经验本身还在。
	var content, cid, taskID string
	if err := sh.DB().QueryRow(`SELECT content, conversation_id, task_id FROM mem_entries LIMIT 1`).
		Scan(&content, &cid, &taskID); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{fakeQQ, "https://", `\nas\share`, "\\\\"} {
		if strings.Contains(content, leak) {
			t.Errorf("正文仍含泄露片段 %q: %s", leak, content)
		}
	}
	if !strings.Contains(content, "计数用文件落盘") {
		t.Errorf("洗得太狠，经验本身没了: %s", content)
	}
	// 会话键：收成单向摘要，且同一原值仍归并到同一个值。
	if !redact.IsOpaqueSessionID(cid) || cid == "" {
		t.Errorf("conversation_id 没收口: %q", cid)
	}
	if want := redact.MustSessionID(legacyCID); cid != want {
		t.Errorf("会话键不稳定: 得到 %q，期望 %q", cid, want)
	}
	// 标识列：不合规的置空，而不是把原样值留着。
	if taskID != "" {
		t.Errorf("不合规 task_id 应置空，得到 %q", taskID)
	}
	// 网节点的样本句也是正文（旧迁移没洗它）。
	var anchor string
	if err := sh.DB().QueryRow(`SELECT anchor FROM mem_nodes WHERE id='n-legacy'`).Scan(&anchor); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(anchor, "\\\\") || redact.Detect(anchor) != "" {
		t.Errorf("anchor 未清洗: %q", anchor)
	}
}

// TestCleanIsIdempotent 洗完再洗一次必须无事可做——否则清理会反复改写同一格。
func TestCleanIsIdempotent(t *testing.T) {
	sh := newLegacyDB(t)
	if _, err := Clean(sh.DB(), true); err != nil {
		t.Fatal(err)
	}
	first := cellDump(t, sh)

	rep, err := Clean(sh.DB(), true)
	if err != nil {
		t.Fatal(err)
	}
	if changed := sumChanged(rep); changed != 0 {
		t.Errorf("第二次清理仍改写 %d 行，清理不稳定", changed)
	}
	if got := cellDump(t, sh); got != first {
		t.Error("第二次清理动了库")
	}
}

// TestReportNeverPrintsRawValue 是清理工具自己的红线：
// 报告里出现 token / 账号，等于把泄露从库里搬到 stdout 和日志里。
func TestReportNeverPrintsRawValue(t *testing.T) {
	sh := newLegacyDB(t)
	rep, err := Clean(sh.DB(), false)
	if err != nil {
		t.Fatal(err)
	}
	out := rep.Summary() + strings.Join(rep.Remaining, "\n")
	for _, leak := range []string{fakeQQ, "sk-abcdefghij012345", "dump.rdb", "attacker.host"} {
		if strings.Contains(out, leak) {
			t.Errorf("报告里出现了原值片段 %q:\n%s", leak, out)
		}
	}
}

// TestApplyRollsBackWhenRecheckNotClean 钉住"洗完复检没归零 → 整事务回滚"那条保险。
//
// 这条分支靠正常数据永远触发不了（clean 与 dirty 同源，存量一定洗得干净），
// 所以这里从 verifyFn 接缝注入残留——它模拟的是真实风险：谓词将来漂移、
// 或新增一列只在一边登记。没有这条测试，那句 `if len(Remaining) > 0` 就是一段
// 谁也踩不到、因而谁也没验过的死代码。
func TestApplyRollsBackWhenRecheckNotClean(t *testing.T) {
	sh := newLegacyDB(t)
	before := cellDump(t, sh)

	// 换掉最后一道闸的检查：报告一处"仍然脏"，改写动作照旧发生。
	// 报告里只给"表.列(类别)"——原值不许进 stdout 的规则同样适用于这里。
	restore := verifyFn
	verifyFn = func(queryer) ([]string, error) {
		return []string{"mem_nodes.anchor(路径)"}, nil
	}
	t.Cleanup(func() { verifyFn = restore })

	rep, err := Clean(sh.DB(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Committed {
		t.Fatal("复检未归零却提交了（半洗的库已经写盘）")
	}
	if len(rep.Remaining) == 0 {
		t.Fatalf("复检结果没带回报告里: %+v", rep)
	}
	if !strings.Contains(rep.Summary(), "未写盘") {
		t.Errorf("摘要要说出没写盘这件事: %s", rep.Summary())
	}
	if got := cellDump(t, sh); got != before {
		t.Error("回滚不彻底：复检未归零时库里留下了改写")
	}

	// 反向对照：把接缝换回真复检，同一份库必须能提交。
	// 少了这一步，上面所有断言可能只是被一个"永远失败的假检查"撑起来的空话。
	verifyFn = restore
	rep2, err := Clean(sh.DB(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Committed || len(rep2.Remaining) != 0 {
		t.Fatalf("真复检下应当洗完提交: %+v", rep2)
	}
}

// TestNullConversationStaysNull：memories.conversation_id 允许 NULL，
// 空值不是泄露，不该被写成空串以外的任何东西。
func TestNullConversationStaysNull(t *testing.T) {
	sh := newLegacyDB(t)
	if _, err := Clean(sh.DB(), true); err != nil {
		t.Fatal(err)
	}
	var cid any
	if err := sh.DB().QueryRow(`SELECT conversation_id FROM memories LIMIT 1`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	switch v := cid.(type) {
	case nil:
	case string:
		if v != "" {
			t.Errorf("NULL 会话键被改写成了 %q", v)
		}
	default:
		t.Errorf("会话键类型变了: %T", cid)
	}
}

func sumChanged(r Report) int {
	n := 0
	for _, t := range r.Tables {
		n += t.Changed
	}
	return n
}

// cellDump 把清单里那些列按固定顺序拼成一个字符串，用来比较"库有没有被动过"。
func cellDump(t *testing.T, sh *shared.Store) string {
	t.Helper()
	var b strings.Builder
	for _, tb := range tables {
		names := make([]string, 0, len(tb.cols))
		for _, c := range tb.cols {
			names = append(names, c.name)
		}
		rows, err := sh.DB().Query(`SELECT rowid, ` + strings.Join(names, ", ") + ` FROM ` + tb.name + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			raw := make([]any, len(tb.cols)+1)
			ptrs := make([]any, len(raw))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			b.WriteString(tb.name)
			for _, v := range raw {
				b.WriteString("\x00" + asString(v))
			}
			b.WriteString("\n")
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}
