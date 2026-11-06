// Package repair 清理共享经验库里的存量泄露。
//
// 为什么需要它：写入边界（redact）只管**以后**的写入，管不着红线立起来之前
// 已经躺在库里的行。2026-11-01 那次实际泄露的 12 + 117 行就是这种存量——
// 只补守卫不清存量，等于把带凭据的那句话继续留在"共享库"里。
//
// 三条自我约束：
//   - 默认预演（dry-run），要写盘必须显式 apply；
//   - 报告只出计数与类别，**绝不打印原值**——把 token 从库里搬到 stdout / 日志里
//     不算修复，算换个地方泄露；
//   - 洗完立刻用同一套谓词复检，命中必须归零，否则报告里带出来。
//     "自称干净"正是这次事故的起因。
package repair

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/NouSart/sump/memory/redact"
)

// kind 是一列的待遇。写在列定义上，不靠列名猜——名字会改，语义也会跟着漂。
type kind int

const (
	kText    kind = iota // 正文：掩码
	kSession             // 会话键：收成「入口 + 摘要」，不合规置空
	kRef                 // 不透明标识：只允许 id 形状，不合规置空
)

type col struct {
	name string
	k    kind
}

type table struct {
	name string
	cols []col
}

// tables 是唯一一份「哪张表哪些列、各按什么待遇」的定义：
// 清理与复检都读它，改一处两边一起变，不会出现"清完了但按另一套标准复检"。
var tables = []table{
	{"memories", []col{
		{"content", kText}, {"kind", kText}, {"source", kRef}, {"conversation_id", kSession},
	}},
	{"mem_entries", []col{
		{"content", kText}, {"task_id", kRef}, {"source", kRef}, {"conversation_id", kSession},
	}},
	// 网节点的 anchor 是"样本句"：种子句是写死的，但旧迁移把 legacy 表原样搬进来过，
	// 里面这一列可能是模型写的句子。漏一列就等于整库复检说了不算。
	{"mem_nodes", []col{
		{"anchor", kText},
	}},
	{"shared_sessions", []col{
		{"id", kSession}, {"entry", kRef}, {"title", kText},
	}},
	{"shared_tasks", []col{
		{"task_id", kRef}, {"session_id", kSession}, {"title", kText}, {"goal", kText}, {"outcome", kText},
		{"capabilities", kText},
	}},
}

// TableStat 是一张表的处理结果。
type TableStat struct {
	Name     string
	Rows     int
	Changed  int
	Columned map[string]int
}

// Report 是一次清理的总结果。
type Report struct {
	Applied   bool // true = 请求写盘
	Committed bool // true = 真的提交了（Applied 但复检未归零时会整事务回滚，此时为 false）
	Tables    []TableStat
	Remaining []string // 复检仍命中的位置（表.列(类别)，不含原值）
}

// Summary 是给运维看的摘要。
func (r Report) Summary() string {
	var b strings.Builder
	switch {
	case r.Committed:
		b.WriteString("已写盘\n")
	case r.Applied:
		b.WriteString("未写盘：复检未归零，整个事务已回滚\n")
	default:
		b.WriteString("预演（未写盘）\n")
	}
	for _, t := range r.Tables {
		cols := make([]string, 0, len(t.Columned))
		for c, n := range t.Columned {
			cols = append(cols, fmt.Sprintf("%s=%d", c, n))
		}
		sort.Strings(cols)
		fmt.Fprintf(&b, "  %-16s 扫描 %d 行，改写 %d 行（%s）\n", t.Name, t.Rows, t.Changed, strings.Join(cols, " "))
	}
	if len(r.Remaining) > 0 {
		fmt.Fprintf(&b, "  复检仍命中：%s\n", strings.Join(r.Remaining, ", "))
	} else {
		b.WriteString("  复检：无命中\n")
	}
	return b.String()
}

// Clean 清理共享经验库。apply=false 时只统计不改写（事务末尾一定回滚）。
//
// 整个清理在一个事务里：半途失败留下"洗了一半"的库，比不洗更难判断。
// 复检必须走同一个事务——共享库是 SetMaxOpenConns(1) 的 SQLite，
// 用 db 另开连接查会在唯一连接上死等，而且看不见事务里未提交的改写。
func Clean(db *sql.DB, apply bool) (Report, error) {
	rep := Report{Applied: apply}
	tx, err := db.Begin()
	if err != nil {
		return rep, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, tb := range tables {
		st, err := cleanTable(tx, tb)
		if err != nil {
			return rep, fmt.Errorf("清理 %s 失败: %w", tb.name, err)
		}
		rep.Tables = append(rep.Tables, st)
	}
	rep.Remaining, err = verifyFn(tx)
	if err != nil {
		return rep, fmt.Errorf("复检失败: %w", err)
	}
	if apply {
		if len(rep.Remaining) > 0 {
			return rep, nil
		}
		if err := tx.Commit(); err != nil {
			return rep, err
		}
		rep.Committed = true
	}
	return rep, nil
}

// cleanTable 扫一张表并（在事务里）改写不合规的格子。
// 预演也走同样的 UPDATE，只是最后由 Clean 回滚——这样"预演报告"和"实洗结果"
// 是同一份逻辑算出来的，不会出现预演说干净、实洗完才发现不干净。
func cleanTable(tx *sql.Tx, tb table) (TableStat, error) {
	st := TableStat{Name: tb.name, Columned: map[string]int{}}
	names := make([]string, 0, len(tb.cols))
	for _, c := range tb.cols {
		names = append(names, c.name)
	}
	rows, err := tx.Query(`SELECT rowid, ` + strings.Join(names, ", ") + ` FROM ` + tb.name)
	if err != nil {
		return st, err
	}
	defer rows.Close()

	type patch struct {
		rowid int64
		cols  []string
		vals  []any
	}
	var patches []patch
	for rows.Next() {
		var rowid int64
		raw := make([]any, len(tb.cols))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(append([]any{&rowid}, ptrs...)...); err != nil {
			return st, err
		}
		p := patch{rowid: rowid}
		for i, c := range tb.cols {
			cur := asString(raw[i])
			next := clean(c.k, cur)
			if next == cur {
				continue
			}
			p.cols = append(p.cols, c.name)
			p.vals = append(p.vals, next)
		}
		if len(p.cols) > 0 {
			patches = append(patches, p)
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	_ = rows.Close()

	if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + tb.name).Scan(&st.Rows); err != nil {
		return st, err
	}
	st.Changed = len(patches)
	for _, p := range patches {
		for _, c := range p.cols {
			st.Columned[c]++
		}
		sets := make([]string, 0, len(p.cols))
		for _, c := range p.cols {
			sets = append(sets, c+" = ?")
		}
		args := append(append([]any{}, p.vals...), p.rowid)
		if _, err := tx.Exec(`UPDATE `+tb.name+` SET `+strings.Join(sets, ", ")+` WHERE rowid = ?`, args...); err != nil {
			return st, fmt.Errorf("改写 %s 的一行失败: %w", tb.name, err)
		}
	}
	return st, nil
}

// clean 把一格洗成合规形状：正文掩码，会话键摘要，标识不合规置空。
// 置空而不是报错：存量数据没有"调用方"可以接收错误，
// 而丢一个分组值远好过丢一条经验，更远好过把账号留在共享库里。
func clean(k kind, cur string) string {
	switch k {
	case kSession:
		if redact.IsOpaqueSessionID(cur) {
			return cur
		}
		opaque, err := redact.SessionID(cur)
		if err != nil {
			return ""
		}
		return opaque
	case kRef:
		if redact.IsRefID(cur) {
			return strings.TrimSpace(cur)
		}
		return ""
	default:
		return redact.Text(cur)
	}
}

// dirty 回答"这一格还有问题吗"，返回值是问题类别（空 = 干净）。
// 用的是与 clean 同一份谓词，所以不存在"洗完了但按另一套标准检查"。
func dirty(k kind, v string) string {
	switch k {
	case kSession:
		if redact.IsOpaqueSessionID(v) {
			return ""
		}
		return "账号未收口"
	case kRef:
		if redact.IsRefID(v) {
			return ""
		}
		return "标识不合规"
	default:
		return redact.Detect(v)
	}
}

// queryer 让复检既能跑在 *sql.DB 上（平时体检），也能跑在 *sql.Tx 里（洗完复检）。
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// verifyFn 是复检的调用点接缝，真实实现就是下面的 verify。
//
// 为什么要有这个变量：Clean 里"复检没归零就整事务回滚"那条 fail-closed 分支，
// 在当前谓词下永远触发不了——clean 与 dirty 同源，正常存量一定能洗完。
// 一条触发不了又没人测的保险等于没有保险（异变测试把 `if len(Remaining) > 0` 改成
// `if false` 时，全套测试原本一声不响）。留这个接缝，测试就能按"谓词将来漂移"
// 这一真实风险形态把分支钉住，而生产行为一个字不变。
var verifyFn = verify

// verify 复检整库：只报"表.列(类别)"，不报原值。
func verify(q queryer) ([]string, error) {
	var bad []string
	for _, tb := range tables {
		names := make([]string, 0, len(tb.cols))
		for _, c := range tb.cols {
			names = append(names, c.name)
		}
		rows, err := q.Query(`SELECT ` + strings.Join(names, ", ") + ` FROM ` + tb.name)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			raw := make([]any, len(tb.cols))
			ptrs := make([]any, len(raw))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return bad, err
			}
			for i, c := range tb.cols {
				if why := dirty(c.k, asString(raw[i])); why != "" {
					bad = append(bad, tb.name+"."+c.name+"("+why+")")
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return bad, err
		}
	}
	sort.Strings(bad)
	return dedup(bad), nil
}

// Verify 单独暴露复检：清完之后再跑一次，或平时当体检用。
func Verify(db *sql.DB) ([]string, error) { return verify(db) }

func asString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprint(s)
	}
}

func dedup(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := xs[:0]
	for i, v := range xs {
		if i == 0 || xs[i-1] != v {
			out = append(out, v)
		}
	}
	return out
}
