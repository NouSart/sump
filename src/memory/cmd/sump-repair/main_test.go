package main

// 这组用例直接调 run()，不起子进程、不碰真实数据：
// 闸门那几个退出码是这条链上唯一"机器替人读库"的结论，必须能在原子上断到。
// 每个用例除了断退出码，还断**自己那一类的输出行**——只断数字的话，
// "把安全结论报成维护问题"（该 novel 的落进 stale）这类错照样绿。

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/memory/audit"
	"github.com/NouSart/sump/memory/shared"
)

// 造的假值：形状同类，不对应任何真实凭据、账号或路径。
const (
	fakeEntropy   = "abCdEf12345GhIjKlMnOp" // 21 位，高熵串会认
	fakeDigits    = "9876543210"            // 10 位账号形状
	fakeKnown     = "a1b2c3d4e5f6g7"        // 14 位，已知凭据比对的假值
	fakeKnownLong = "a1b2c3d4e5f6g7h8i9"    // 20 位，redact 的上下文规则洗得掉的形状
)

// scopeDB 按 audit.Scope() 派生最小表结构：只给 -audit 这条路用（它只读正文列，
// 靠 SQLite 隐式 rowid 定位），不需要 repair 的标识列。
// 表与列全部从 Scope() 现场取，不在此手写第二份清单——清单漂了这里跟着漂才有意义。
func scopeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	columns := map[string][]string{}
	var tables []string
	for _, col := range audit.Scope() {
		if _, ok := columns[col.Table]; !ok {
			tables = append(tables, col.Table)
		}
		columns[col.Table] = append(columns[col.Table], col.Name)
	}
	sort.Strings(tables)
	for _, table := range tables {
		defs := make([]string, 0, len(columns[table]))
		for _, name := range columns[table] {
			defs = append(defs, name+" TEXT")
		}
		if _, err := db.Exec(`CREATE TABLE ` + table + ` (` + strings.Join(defs, ", ") + `)`); err != nil {
			t.Fatalf("建 %s 失败: %v", table, err)
		}
	}
	return path
}

// insertDirty 往 memories.content 塞一格脏文本，返回它的 rowid。
func insertDirty(t *testing.T, dbPath, text string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`INSERT INTO memories(content) VALUES(?)`, text)
	if err != nil {
		t.Fatal(err)
	}
	rowKey, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return rowKey
}

// writeFile 落一个临时文件（白名单 JSON 之类），返回路径。
func writeFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// adjudicatedJSON 把裁定条目编成 JSON；条目由测试从真实 Finding 造，键不外泄任何东西。
func adjudicatedJSON(t *testing.T, entries ...audit.Adjudicated) string {
	t.Helper()
	if entries == nil {
		entries = []audit.Adjudicated{}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "adjudicated.json", raw)
}

// runWith 跑一次 run()，返回退出码与两条输出流。
func runWith(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// 1. 无白名单的脏库：-no-adjudicated 必须把命中原样报出来并返回 1。
// 这条钉的是基线性质——修前的泄漏行从未被裁定，关掉白名单就必破闸。
func TestNoAdjudicatedBreaksOnDirtyDB(t *testing.T) {
	dbPath := scopeDB(t)
	rowKey := insertDirty(t, dbPath, "轮转后 "+fakeEntropy+" 与 "+fakeDigits+" 两条形状")

	code, out, errOut := runWith(t, "-audit", "-no-adjudicated", "-db", dbPath)
	if code != 1 {
		t.Fatalf("脏库关掉白名单该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	want := "memories." + itoa(rowKey) + ".content(高熵串"
	if !strings.Contains(out, want) {
		t.Errorf("输出里没有新命中行 %q：%s", want, out)
	}
	if !strings.Contains(out, "-no-adjudicated：不查已裁定白名单") {
		t.Errorf("没说明这是关掉白名单的口径：%s", out)
	}
	if !strings.Contains(errOut, "第二把尺不归零") {
		t.Errorf("stderr 缺少破闸引导语：%s", errOut)
	}
}

// 2. 干净库 + 默认白名单：命中为 0，默认那两条裁定只该归 skipped，不得误报 stale。
// "行不存在算 skipped 不算 stale"是 28684c3 那条改准的口径，这里机器钉住。
func TestCleanDBWithDefaultWhitelistPasses(t *testing.T) {
	dbPath := scopeDB(t)
	adj, err := audit.DefaultAdjudicated()
	if err != nil {
		t.Fatalf("内置白名单读不动: %v", err)
	}

	code, out, errOut := runWith(t, "-audit", "-db", dbPath)
	if code != 0 {
		t.Fatalf("干净库该返回 0，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if strings.Contains(out, "过期裁定条目") {
		t.Errorf("库里压根没有那些行，不该报过期：%s", out)
	}
	// 条数现场从内置那份解析，不写死：白名单加一条这里跟着走。
	wantSkip := "跳过 " + strconv.Itoa(len(adj)) + " 条已裁定记录"
	if !strings.Contains(out, wantSkip) {
		t.Errorf("输出里缺少 %q：%s", wantSkip, out)
	}
	if !strings.Contains(out, "命中 0 处，全部已裁定") {
		t.Errorf("干净库该报成全部已裁定（0 处）：%s", out)
	}
}

// 3. 两个开关同时给：必须在打开数据库之前就报互斥。
// 断退出码不够——删掉互斥检查后，默认 -db 的 os.Stat 失败也会返回 2，
// 那条 2 会把这条守卫冒充成"已经验过"。所以同时断 stderr 的文案与"没走到打不开库"。
func TestAdjudicatedFlagsAreMutuallyExclusive(t *testing.T) {
	adjPath := adjudicatedJSON(t)
	missingDB := filepath.Join(t.TempDir(), "does-not-exist.db")

	code, out, errOut := runWith(t, "-audit", "-adjudicated", adjPath, "-no-adjudicated", "-db", missingDB)
	if code != 2 {
		t.Fatalf("两标志同给该返回 2，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "互斥") {
		t.Errorf("stderr 里没有互斥说明，这条 2 可能是别的原因：%s", errOut)
	}
	if strings.Contains(errOut, "打不开共享库") {
		t.Errorf("互斥检查该在打开数据库之前，实际走到了开库：%s", errOut)
	}
}

// 4. stale 单独成立：库里有 0 命中（novel 必空），但白名单点着的行还在、键对不上。
// 这个场景里"删除 stale 失败退出"必须能被单独杀死——别处没有第二个 1 可兜底。
func TestStaleAloneBreaksGate(t *testing.T) {
	dbPath := scopeDB(t)
	rowKey := insertDirty(t, dbPath, "一条正常的提炼物，不带任何凭据形状")

	adj := []audit.Adjudicated{{
		Table: "memories", RowKey: rowKey, Column: "content", Probe: audit.ProbeEntropy,
		Key: "00000000", Runs: 1, Lens: []int{99}, Reason: "行在库里但当前命中没有这个键",
	}}
	code, out, errOut := runWith(t, "-audit", "-db", dbPath, "-adjudicated", adjudicatedJSON(t, adj...))
	if code != 1 {
		t.Fatalf("过期裁定该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "过期裁定条目") {
		t.Errorf("输出里没有过期条目：%s", out)
	}
	if !strings.Contains(out, "未裁定 0 处") {
		t.Errorf("这个场景 novel 该是 0，别把安全结论混进来：%s", out)
	}
	if !strings.Contains(errOut, "白名单里有对不上当前库的条目") {
		t.Errorf("stderr 缺少过期引导语：%s", errOut)
	}
}

// 5. 未知开关：flag 的错误与 Usage 都要落进注入的 stderr（SetOutput 那条要求的实证）。
func TestUnknownFlagReturnsTwo(t *testing.T) {
	code, out, errOut := runWith(t, "-不存在的开关")
	if code != 2 {
		t.Fatalf("未知开关该返回 2，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "not defined") {
		t.Errorf("flag 错误没进注入的 stderr：%q", errOut)
	}
	if !strings.Contains(errOut, "Usage of sump-repair") {
		t.Errorf("Usage 没进注入的 stderr：%q", errOut)
	}
	if out != "" {
		t.Errorf("用法错误不该往 stdout 写东西：%q", out)
	}
}

// 6. novel 单独成立：白名单为空数组，脏库全部命中归 novel。
// 这里 stale/skipped 都是 0，"删除 novel 失败退出"只能被这条杀死。
func TestNovelAloneBreaksGate(t *testing.T) {
	dbPath := scopeDB(t)
	rowKey := insertDirty(t, dbPath, "令牌 "+fakeKnown+" 另存 "+fakeDigits)

	// 这一格文本同时命中两路（密钥上下文与长数字），novel 该是 2；
	// 真正要断的是"有 novel 就破闸"以及 stale/skipped 全空，别处没有第二个 1 可兜底。
	code, out, errOut := runWith(t, "-audit", "-db", dbPath, "-adjudicated", adjudicatedJSON(t))
	if code != 1 {
		t.Fatalf("空白名单下的命中该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "未裁定 2 处") {
		t.Errorf("未裁定计数不对：%s", out)
	}
	if !strings.Contains(out, "过期裁定 0 处") || strings.Contains(out, "跳过 ") {
		t.Errorf("这个场景 stale 与 skipped 都该是 0：%s", out)
	}
	want := "memories." + itoa(rowKey) + ".content(长数字"
	if !strings.Contains(out, want) {
		t.Errorf("没有打印新命中行 %q：%s", want, out)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// hitsOf 用第二把尺读一遍临时库：测试据此造裁定，键走的是同一条 Key() 规则，
// 不是手编的（真实流程里人也只能从 -audit 的输出抄 key）。
func hitsOf(t *testing.T, dbPath string) []audit.Finding {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	hits, err := audit.Scan(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// entryAt 把命中登记成裁定条目，再只改位置字段（键保持命中算出来的那个）。
// 冲突夹具必须这么造：从实测 Finding 取键，不重算摘要、不写真实凭据。
func entryAt(f audit.Finding, rowKey int64, reason string) audit.Adjudicated {
	a := adjudicatedFrom(f, reason)
	a.RowKey = rowKey
	return a
}

// adjudicatedFrom 与 audit 包里的测试助手同名同义：cmd 测试拿不到包内函数，这里重写一份。
func adjudicatedFrom(f audit.Finding, reason string) audit.Adjudicated {
	return audit.Adjudicated{
		Table: f.Table, RowKey: f.RowKey, Column: f.Column, Probe: f.Probe,
		Key: f.Key(), Runs: f.Shape.Runs, Lens: f.Shape.Lens, Ent: f.Shape.Ent, Reason: reason,
	}
}

// 8. conflicted 单独成立：键认下了、位置对不上，novel / stale / skipped 全为 0。
// 这条场景必须能单独杀死"删除 conflicted 失败退出"的异变——删掉之后这里返回 0。
func TestConflictedAloneBreaksGate(t *testing.T) {
	dbPath := scopeDB(t)
	insertDirty(t, dbPath, "轮转后 "+fakeEntropy)
	hits := hitsOf(t, dbPath)
	if len(hits) != 1 {
		t.Fatalf("夹具该恰好造出一处命中，实际 %d 处", len(hits))
	}
	adj := []audit.Adjudicated{entryAt(hits[0], 999999, "同一条键，行号被改过")}

	code, out, errOut := runWith(t, "-audit", "-db", dbPath, "-adjudicated", adjudicatedJSON(t, adj...))
	if code != 1 {
		t.Fatalf("位置冲突该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "位置冲突 1 处") {
		t.Errorf("计数里没有位置冲突这一类：%s", out)
	}
	if !strings.Contains(out, "位置冲突命中：") || !strings.Contains(out, "白名单把这条键记在") {
		t.Errorf("没把冲突条目的两个位置都打出来（人要靠这个核对）：%s", out)
	}
	if !strings.Contains(out, "未裁定 0 处") || !strings.Contains(out, "过期裁定 0 处") || strings.Contains(out, "跳过 ") {
		t.Errorf("这个场景 novel / stale / skipped 都该是 0：%s", out)
	}
	if !strings.Contains(errOut, "键对得上、位置对不上") {
		t.Errorf("stderr 缺少冲突引导语：%s", errOut)
	}
}

// 9. novel 与 stale 同时成立：两类条目、两句引导语都要出现，最后才返回 1。
// 早退（novel 判完就 return）会把 stale 的提示吞掉——这条就是盯着这个改法。
func TestNovelAndStaleReportedTogether(t *testing.T) {
	dbPath := scopeDB(t)
	dirty := insertDirty(t, dbPath, "轮转后 "+fakeEntropy)
	// 另插一行干净的：它的 rowid 存在，但当前没有任何命中对得上那条假键 → stale。
	clean := insertDirty(t, dbPath, "一条正常的提炼物，不带任何凭据形状")
	adj := []audit.Adjudicated{{
		Table: "memories", RowKey: clean, Column: "content", Probe: audit.ProbeEntropy,
		Key: "00000000", Runs: 1, Lens: []int{9}, Reason: "行还在库里但键已对不上",
	}}

	code, out, errOut := runWith(t, "-audit", "-db", dbPath, "-adjudicated", adjudicatedJSON(t, adj...))
	if code != 1 {
		t.Fatalf("未裁定 + 过期该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	wantNew := "memories." + itoa(dirty) + ".content(高熵串"
	if !strings.Contains(out, wantNew) {
		t.Errorf("没打印新命中：%s", out)
	}
	if !strings.Contains(out, "过期裁定条目") {
		t.Errorf("没打印过期条目：%s", out)
	}
	if !strings.Contains(errOut, "有未裁定的命中") || !strings.Contains(errOut, "白名单里有对不上当前库的条目") {
		t.Errorf("两句引导语该一起出现：%s", errOut)
	}
}

// 10. 三类同时成立：全部输出后才退出，一个都不能被另一个挡住。
func TestAllThreeCategoriesReportedTogether(t *testing.T) {
	dbPath := scopeDB(t)
	clashRow := insertDirty(t, dbPath, "轮转后 "+fakeEntropy)
	novelRow := insertDirty(t, dbPath, "联系 "+fakeDigits+" 处理")
	// 取冲突那条的键：位置改成不存在的行号。
	var clashKey string
	for _, h := range hitsOf(t, dbPath) {
		if h.RowKey == clashRow && h.Probe == audit.ProbeEntropy {
			clashKey = h.Key()
		}
	}
	if clashKey == "" {
		t.Fatal("没造出用于冲突的命中，夹具失效")
	}
	adj := []audit.Adjudicated{
		{Table: "memories", RowKey: 999999, Column: "content", Probe: audit.ProbeEntropy,
			Key: clashKey, Runs: 1, Lens: []int{21}, Reason: "行号被改过的同键裁定"},
		{Table: "memories", RowKey: novelRow, Column: "content", Probe: audit.ProbeEntropy,
			Key: "00000000", Runs: 1, Lens: []int{9}, Reason: "行在库里、键对不上 → stale"},
	}

	code, out, errOut := runWith(t, "-audit", "-db", dbPath, "-adjudicated", adjudicatedJSON(t, adj...))
	if code != 1 {
		t.Fatalf("三类并存该返回 1，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	for _, want := range []string{
		"memories." + itoa(novelRow) + ".content(长数字", // novel
		"位置冲突命中：",                                     // conflicted
		"过期裁定条目",                                      // stale
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里缺 %q：%s", want, out)
		}
	}
	for _, want := range []string{"有未裁定的命中", "键对得上、位置对不上", "白名单里有对不上当前库的条目"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr 缺引导语 %q：%s", want, errOut)
		}
	}
	if !strings.Contains(out, "未裁定 1 处、位置冲突 1 处、过期裁定 1 处") {
		t.Errorf("三类计数该各 1 处：%s", out)
	}
}

// 7. -apply 路径：只在临时库上验。必须用 shared.Open 建**完整真实 schema**——
// Scope 派生的最小表缺 repair 要读的标识列（source / conversation_id / task_id /
// shared_sessions.entry / shared_tasks.session_id 等），拿它跑 -apply 只会撞出一个假失败，
// 而不是证明清洗与退出码。脏格用 redact 洗得掉的形状（与 shared 里 smuggleCleanable 同类）。
func TestApplyCleansTempLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experience.db")
	s, err := shared.Open(path, time.Now())
	if err != nil {
		t.Fatalf("建完整 schema 失败: %v", err)
	}
	const dirty = "把 NapCat 令牌 " + fakeKnownLong + " 写成默认"
	res, err := s.DB().Exec(`INSERT INTO memories(kind, content, priority, source, conversation_id, status, created_at, updated_at)
		VALUES('事实', ?, 1, 'reasoner', '', 'active', '2026-11-06T00:00:00Z', '2026-11-06T00:00:00Z')`, dirty)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	rowKey, err := res.LastInsertId()
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runWith(t, "-db", path, "-apply")
	if code != 0 {
		t.Fatalf("-apply 洗完该返回 0，实际 %d\nstdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "已写盘") {
		t.Errorf("repair 摘要没进注入的 stdout（跑的全局输出流？）：%s", out)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var got string
	if err := db.QueryRow(`SELECT content FROM memories WHERE rowid = ?`, rowKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, fakeKnownLong) {
		t.Errorf("洗完仍留着原值：%q", got)
	}
	if !strings.Contains(got, "[已隐去]") {
		t.Errorf("洗完没看到掩码标记，清洗可能压根没动这一格：%q", got)
	}
	hits, err := audit.Scan(db, []string{fakeKnownLong})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		var lines []string
		for _, h := range hits {
			lines = append(lines, h.String())
		}
		t.Errorf("洗完第二把尺仍命中：%s", strings.Join(lines, ", "))
	}
}
