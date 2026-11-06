package audit

// 已裁定白名单：闸门从"有命中就破"改成"有**未裁定**的命中才破"。
//
// 为什么要有它：探针报出的形状里有一类是永久误报（经验里合法的长驼峰标识符），
// redact 洗不掉、值判别器还没做。以前每次跑 -audit 都要人重读同一份清单，
// "看过了"这件事没有任何地方记得——于是要么忍受长红，要么改弱探针。
// 后者是红线，所以改成前者：把逐处读过的结论登记成一份**摘要**白名单。
//
// 白名单存摘要而不是存原值：这里没有一处能从库里搬出正文，key 是形状的指纹。
// 抄录方式只有一种——从 Finding.String() 的输出里逐字抄 key（adjudicated.json 不是手写的）。
//
// 本文件同样受包注释里那条独立性硬约束管辖：不得 import redact / shared / repair。

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Adjudicated 是一条"人已逐处读过、决定保留"的裁定记录。
//
// 判定**只认 Key**。Runs / Lens / Ent 是当初 -audit 输出里那三列形状的同样副本，
// 只为了让人不必再跑一次工具也能读懂这一格，不参与任何比较。
// 特别地：不许从 Lens / Ent 重算摘要——规范编码与取整时机两处必须和 Finding.Key()
// 逐字一致，而这两处一旦漂开**不会有任何测试变红**（重算的和抄的都自洽），
// 只有真跑闸门才知道，那时它已经放行了不该放行的东西。key 是抄进来的，不是算出来的。
type Adjudicated struct {
	Table  string    `json:"table"`
	RowKey int64     `json:"row_key"`
	Column string    `json:"column"`
	Probe  string    `json:"probe"`
	Key    string    `json:"key"`
	Runs   int       `json:"runs"`
	Lens   []int     `json:"lens"`
	Ent    []float64 `json:"ent"`
	Reason string    `json:"reason"`
}

// String 是人读的一行（过期时打印它，而不是打印一个孤零零的 key）。
func (a Adjudicated) String() string {
	return fmt.Sprintf("%s.%d.%s(%s key=%s)", a.Table, a.RowKey, a.Column, a.Probe, a.Key)
}

// 编进二进制的默认白名单：随代码走、随评审走，换了库也不会带着机器上的私人裁定。
// 生成方式见 -no-adjudicated 那条引导顺序（包外文档 / 交付说明里写清）。
//
//go:embed adjudicated.json
var embeddedAdjudicated []byte

// DefaultAdjudicated 返回内置白名单。解析失败 = 仓库里这份文件坏了，
// 调用方必须报错退出，不许当成"白名单为空"继续跑（那等于闸门自己拆了自己）。
func DefaultAdjudicated() ([]Adjudicated, error) {
	return ParseAdjudicated(embeddedAdjudicated)
}

// LoadAdjudicated 从指定路径读一份覆盖白名单（-adjudicated）：外来库、别的机器上的库
// 用得起自己的裁定清单，不必重编二进制。
func LoadAdjudicated(path string) ([]Adjudicated, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读已裁定白名单 %s 失败: %w", path, err)
	}
	list, err := ParseAdjudicated(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list, nil
}

// ParseAdjudicated 解析并做合法性收口。四条都是"错了就整份不认"，因为一份
// 半认半废的白名单比没有白名单更危险：人会以为某处已经裁过了。
//
//	table / column 必须落在 Scope() 的清单里——这一条同时是安全边界（表名要拼进
//	SELECT rowid 的语句，只允许清单内的标识符）与防自欺（扫不到的位置永远配不上命中，
//	写在白名单里只是把漏扫伪装成已裁定）。
func ParseAdjudicated(raw []byte) ([]Adjudicated, error) {
	var list []Adjudicated
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析已裁定白名单失败: %w", err)
	}
	seen := make(map[string]bool, len(list))
	for i, a := range list {
		if a.Key == "" {
			return nil, fmt.Errorf("白名单第 %d 条（%s.%d.%s）没有 key：key 只能从 -audit 输出逐字抄，不许手编",
				i+1, a.Table, a.RowKey, a.Column)
		}
		if seen[a.Key] {
			return nil, fmt.Errorf("白名单第 %d 条的 key %s 重复", i+1, a.Key)
		}
		seen[a.Key] = true
		if strings.TrimSpace(a.Reason) == "" {
			return nil, fmt.Errorf("白名单 %s 没有 reason：写明为什么保留，才是裁定；空理由等于没裁", a.Key)
		}
		if !isProbe(a.Probe) {
			return nil, fmt.Errorf("白名单 %s 的 probe %q 不是四路探针之一（探针改名是破坏性变更，白名单要一起改）", a.Key, a.Probe)
		}
		if !inScope(a.Table, a.Column) {
			return nil, fmt.Errorf("白名单 %s 指的 %s.%s 不在复检清单里：扫不到的位置没有可裁定的命中，这条只会把漏扫伪装成已裁定", a.Key, a.Table, a.Column)
		}
	}
	return list, nil
}

// isProbe 判类别名是否是那四路之一。
func isProbe(name string) bool {
	switch name {
	case ProbeKnown, ProbeContext, ProbeEntropy, ProbeLongDigit:
		return true
	}
	return false
}

// inScope 判（表，列）是否在扫描清单里。
func inScope(table, column string) bool {
	for _, c := range scope {
		if c.Table == table && c.Name == column {
			return true
		}
	}
	return false
}

// Filter 拿白名单对账，四分类：
//
//	novel      —— 命中里键没被白名单认下的：未裁定，**安全**问题，必须破闸。
//	conflicted —— 键认下了、但裁定条目记的位置（表 / 行 / 列 / 探针）与实测命中不一致：
//	              这条键是从别的格子抄来的，或者格子被搬过。**既不放行也不重复计 novel**——
//	              放行等于让一份错位清单替当前库背书，计 novel 又把"清单本身坏了"报成新泄漏。
//	stale      —— 白名单条目里行还在库里、却对不上当前任何一条同键命中的：行被改写过，
//	              或者这一格已经被洗掉了。**维护**问题，同样破闸，但错误信息与 novel 分开。
//	skipped    —— 白名单条目里行已经不在了（洗完删过行，或换了一份 schema 相同、内容不同的库）：
//	              不是错误，只打一行提示。
//
// 键匹配上的裁定条目**即视为已消耗**，不再参与 stale / skipped 分类——哪怕位置不一致产生了
// conflicted。同一条裁定不能既报"位置不符"又报"这行没了"，两个结论会互相稀释。
// 比较只看 Table / RowKey / Column / Probe 四项位置；Runs / Lens / Ent / Reason 一律不参与，
// 也绝不从它们重算键（理由见 Adjudicated 的注释）。
//
// rowExists 对"整张表都不存在"也必须返回 false 而不是报错：白名单是跟着代码走的，
// 库却是各跑各的——同一份清单遇到换了 schema 的库时，答案该是"这些条目在当前库里
// 对不上任何行"，不是一句 no such table 把整次运行打死（崩掉会连本次的 novel 一起吞了）。
// 行不存在的条目归 skipped，闸门看 novel、conflicted 与 stale。
//
// 匹配的是键，不是位置：同格形状一变，键就变，旧条目立刻成为 stale 而新命中成为 novel，
// 两处一起响。这是"同格新增过阈段不许被旧裁定静默吸收"的落点。
func Filter(hits []Finding, adj []Adjudicated, rowExists func(table string, rowKey int64) bool) (
	novel []Finding, stale, skipped []Adjudicated, conflicted []Finding) {
	adjudged := make(map[string]Adjudicated, len(adj))
	for _, a := range adj {
		adjudged[a.Key] = a // ParseAdjudicated 收口过键唯一，同键不会互相覆盖出歧义
	}
	consumed := make(map[string]bool, len(hits))
	for _, f := range hits {
		k := f.Key()
		entry, ok := adjudged[k]
		if !ok {
			novel = append(novel, f)
			continue
		}
		consumed[k] = true
		if entry.Table == f.Table && entry.RowKey == f.RowKey &&
			entry.Column == f.Column && entry.Probe == f.Probe {
			continue // 位置也一致：已裁定，放行
		}
		conflicted = append(conflicted, f)
	}
	for _, a := range adj {
		if consumed[a.Key] {
			continue
		}
		if rowExists(a.Table, a.RowKey) {
			stale = append(stale, a)
		} else {
			skipped = append(skipped, a)
		}
	}
	byLocation := func(xs []Adjudicated) {
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].Table != xs[j].Table {
				return xs[i].Table < xs[j].Table
			}
			if xs[i].RowKey != xs[j].RowKey {
				return xs[i].RowKey < xs[j].RowKey
			}
			if xs[i].Column != xs[j].Column {
				return xs[i].Column < xs[j].Column
			}
			return xs[i].Probe < xs[j].Probe
		})
	}
	byLocation(stale)
	byLocation(skipped)
	return novel, stale, skipped, conflicted
}

// AdjudicatedAt 按键找回白名单里那条裁定记录的位置，供输出把"记的位置"与"实测的位置"并排给人读。
// 只查位置，不参与任何判定。
func AdjudicatedAt(adj []Adjudicated, key string) (Adjudicated, bool) {
	for _, a := range adj {
		if a.Key == key {
			return a, true
		}
	}
	return Adjudicated{}, false
}

// RowSet 回答"白名单指的这一行还在不在当前库里"，是 Filter 的 rowExists 实现。
//
// 分两步取，第一步是关键：先用一次 sqlite_master 拿到库里**有哪些表**，之后只对
// 白名单点到、且确实存在的表查 rowid。直接对一张不存在的表 SELECT 会抛 no such table，
// 那个错误会顺着 Filter 冒上来把整次运行打死——表不存在的答案是 false（归 skipped），
// 不是错误。注意 Scan 本身仍要求 scope 里每张表都在（缺表当场报错，见 sump-repair 的
// 用法注释与 TODO.md），所以今天真正出声的路径是"表都在、个别行没了"。
//
// 表名来自 Adjudicated，已在 ParseAdjudicated 里收口成 Scope() 内的标识符，
// 拼进语句是安全的；rowid 一律走参数。
type RowSet struct {
	rowids map[string]map[int64]bool
}

// NewRowSet 装载 RowSet。adj 为空时不查库，直接返回一个恒 false 的判定器。
func NewRowSet(db *sql.DB, adj []Adjudicated) (*RowSet, error) {
	set := &RowSet{rowids: make(map[string]map[int64]bool)}
	if len(adj) == 0 {
		return set, nil
	}
	tables, err := existingTables(db)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string][]int64)
	for _, a := range adj {
		if tables[a.Table] {
			wanted[a.Table] = append(wanted[a.Table], a.RowKey)
		}
	}
	for table, rowKeys := range wanted {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rowKeys)), ",")
		args := make([]any, 0, len(rowKeys))
		for _, k := range rowKeys {
			args = append(args, k)
		}
		rows, err := db.Query(`SELECT rowid FROM `+table+` WHERE rowid IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("查 %s 的行是否存在失败: %w", table, err)
		}
		ids := make(map[int64]bool)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("读 %s 的 rowid 失败: %w", table, err)
			}
			ids[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("遍历 %s 的 rowid 失败: %w", table, err)
		}
		rows.Close()
		set.rowids[table] = ids
	}
	return set, nil
}

// Has 判一行在不在。表不在库里时返回 false，不返回错误（见类型注释）。
func (r *RowSet) Has(table string, rowKey int64) bool {
	return r.rowids[table][rowKey]
}

// existingTables 拿库里现有的表名集合。sqlite_master 读不到就直接报错——
// 那种情况下"表不存在"和"库读不动"分不开，宁可停下。
func existingTables(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("读 sqlite_master 失败: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读 sqlite_master 的表名失败: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}
