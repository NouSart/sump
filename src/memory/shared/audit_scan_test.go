package shared

// 第二把尺对**真实 schema** 的验收。
//
// audit 的表/列清单是照着 repair 手抄的第二份（独立性代价），抄错有两种后果：
// 扫一个不存在的列（当场报错），或者某张表压根没被看（不报错，静默少一把尺）。
// 后者只能靠"用生产同一份 DDL 建出来的库真跑一遍 Scan"来兜，所以这个文件留在 shared 包：
// 它用 shared.Open 建库，audit 反过来谁也不 import——依赖方向和包注释里的约束一致。

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/audit"
	"github.com/NouSart/sump/memory/repair"
)

// 造的 10 位账号形状：不是任何真实号码。
const fakeAccount10 = "1234567890"

func TestAuditSeedLibraryIsQuiet(t *testing.T) {
	s := openTest(t)
	// 尺子一上手就冒红 = 它把正常提炼物当泄露，后面所有计数都不可信。
	hits, err := audit.Scan(s.DB(), nil)
	if err != nil {
		t.Fatalf("扫种子库失败（Scope 里的表/列与真实 DDL 对不上？）: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("种子库不该命中：%s", joinFindings(hits))
	}
}

func TestAuditFindsRawLegacyRows(t *testing.T) {
	s := openTest(t)
	smuggle(t, s)

	hits, err := audit.Scan(s.DB(), []string{fakeTokenBare})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Table+"."+h.Column+"("+h.Probe+")"] = true
	}
	for _, want := range []string{
		"memories.content(已知凭据)",
		"memories.content(密钥上下文)",
		"mem_entries.content(长数字)",
		"mem_nodes.anchor(密钥上下文)",
		"shared_sessions.title(长数字)",
		"shared_tasks.title(密钥上下文)",
		"shared_tasks.goal(高熵串)",
		"shared_tasks.outcome(已知凭据)",
	} {
		if !got[want] {
			t.Errorf("没报出 %s，实际：%s", want, joinFindings(hits))
		}
	}
	// 报告里不许出现原值——把凭据从库里搬到 stdout 是换个地方泄露。
	// 查全部塞进去的值，不只令牌那一个：Finding 以后多带一个字段（比如 excerpt）就会当场露出来，
	// 而只 grep 令牌等于假装其余形状都无害。
	report := joinFindings(hits)
	for _, v := range append(append([]string{}, leaky...), fakeAccount10) {
		if strings.Contains(report, v) {
			t.Errorf("命中清单里出现了原值 %q：%s", v, report)
		}
	}
}

// 两把尺要能收口：清洗写完（repair 的谓词归零）之后，独立那把也必须归零。
// 这里只塞"redact 洗得掉"的形状——洗不掉的另说，见下一个用例。
func TestTwoRulersCloseAfterClean(t *testing.T) {
	s := openTest(t)
	smuggleCleanable(t, s)

	rep, err := repair.Clean(s.DB(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Committed {
		t.Fatalf("第一把尺没归零，事务回滚了：%s", rep.Summary())
	}
	hits, err := audit.Scan(s.DB(), []string{fakeTokenBare})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("洗完第二把尺仍命中：%s", joinFindings(hits))
	}
}

// 尺子比清洗宽，是它存在的理由：这一格 redact 认不出（没有形状可言），
// repair 洗完仍自称干净，而 audit 报出来。写成人读项，不写成 bug。
func TestAuditReportsWhatRedactCannotClean(t *testing.T) {
	s := openTest(t)
	if _, err := s.DB().Exec(
		`INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		 VALUES('事实', ?, 1, 'reasoner', '', 'active', '2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z')`,
		"默认值是 "+fakeAccount10+"，另存了一份在配置里",
	); err != nil {
		t.Fatal(err)
	}

	rep, err := repair.Clean(s.DB(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Remaining) > 0 {
		t.Fatalf("第一把尺这里不该有命中：%s", rep.Summary())
	}
	hits, err := audit.Scan(s.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("redact 认不出的裸账号形状，audit 也没报——第二把尺形同虚设")
	}

	// 反过来也得成立：已知凭据探针在手里有真值时能认出同一格。
	withKnown, err := audit.Scan(s.DB(), []string{fakeAccount10})
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, h := range withKnown {
		if h.Probe == audit.ProbeKnown {
			seen = true
		}
	}
	if !seen {
		t.Errorf("给了已知凭据清单却没报 %s，实际：%s", audit.ProbeKnown, joinFindings(withKnown))
	}
}

// smuggle 用裸 SQL 往各张表塞"守卫之前写下的历史数据"——走 Add / Attach / Publish
// 会被写入边界洗掉，那样测不到尺子。
func smuggle(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	if _, err := s.Publish(now, PublishInput{
		SessionID: "web:private:owner", TaskID: "t-smug", Goal: "一条正常的摘要",
	}); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, `UPDATE shared_tasks SET title = ?, goal = ?, outcome = ? WHERE task_id = 't-smug'`,
		"密钥：a1b2c3d4e5f6g7h8i9", "轮转后 abCdEf12345GhIjKlMnOp", "备用值 "+fakeTokenBare)
	mustUpdate(t, s, `UPDATE shared_sessions SET title = ? WHERE id = ?`,
		"运维 10 位账号 "+fakeAccount10, mustOpaque(t, "web:private:owner"))

	mustExec(t, s, `INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		VALUES('事实', ?, 1, 'reasoner', '', 'active', '2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z')`,
		"把 NapCat 令牌 "+fakeTokenBare+" 写成默认")
	mustExec(t, s, `INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
		last_hit_at, created_at, embedding, embedding_model)
		VALUES('e-smug', ?, '', '', 'reasoner', 'durable', 1, 'active',
		'2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z', ?, 'fake')`,
		"联系 "+fakeAccount10+" 处理", []byte{0, 0, 128, 63})
	mustUpdate(t, s, `UPDATE mem_nodes SET anchor = ? WHERE id = 'seed-pref'`, "密码：a1b2c3d4e5f6g7")
}

// smuggleCleanable 与上面同形，但每一格都是 redact 洗得掉的形状。
func smuggleCleanable(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	if _, err := s.Publish(now, PublishInput{
		SessionID: "web:private:owner", TaskID: "t-close", Goal: "一条正常的摘要",
	}); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, `UPDATE shared_tasks SET title = ?, goal = ?, outcome = ? WHERE task_id = 't-close'`,
		"令牌 a1b2c3d4e5f6g7h8i9", "api_key = z9Y8x7W6v5U4t3S2r1", "日志见 /srv/app/logs/app.log")
	mustUpdate(t, s, `UPDATE shared_sessions SET title = ? WHERE id = ?`,
		"联系 13812345678", mustOpaque(t, "web:private:owner"))

	mustExec(t, s, `INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		VALUES('事实', ?, 1, 'reasoner', '', 'active', '2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z')`,
		"把 NapCat 令牌 a1b2c3d4e5f6g7h8i9 写成默认")
	mustExec(t, s, `INSERT INTO mem_entries(id, content, conversation_id, task_id, source, durability, hits, status,
		last_hit_at, created_at, embedding, embedding_model)
		VALUES('e-close', ?, '', '', 'reasoner', 'durable', 1, 'active',
		'2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z', ?, 'fake')`,
		"密钥：a1b2c3d4e5f6g7h8i9，联系 13812345678", []byte{0, 0, 128, 63})
	mustUpdate(t, s, `UPDATE mem_nodes SET anchor = ? WHERE id = 'seed-pref'`, "密码：a1b2c3d4e5f6g7h8i9")
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(query, args...); err != nil {
		t.Fatalf("%s\n: %v", query, err)
	}
}

// mustUpdate 要求恰好改到一行：WHERE 打偏（列名 / 种子 id 变了）会静默通过，
// 那正是"少扫一张表还不报错"的形态。
func mustUpdate(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	res, err := s.DB().Exec(query, args...)
	if err != nil {
		t.Fatalf("%s\n: %v", query, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s 改到 %d 行，期望 1 行（WHERE 打偏了？）", query, n)
	}
}

func joinFindings(hits []audit.Finding) string {
	if len(hits) == 0 {
		return "（无命中）"
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		// 用 Finding.String() 而不是在这里自己拼一遍：sump-repair 打印走的就是它，
		// 自己拼等于把"报告里不许有原值"这条断言变成永远为真——本用例就是这么个假断言，
		// 给 Finding 加一个带原值的字段后它照样绿。
		out = append(out, h.String())
	}
	return strings.Join(out, ", ")
}
