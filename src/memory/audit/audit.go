// Package audit 是共享经验库的第二把尺：独立复检，不参与清洗。
//
// 为什么要有第二把尺：repair 洗完之后回答"干净了吗"的，是**同一套谓词**
// （clean 与 dirty 同源，见 repair 包）。同源就有共同犯错的空间——谓词漏认一类形状，
// 清洗漏掉同一类，复检再报干净，整条链一句老实话都没有。2026-11-06 实测的两条
// 存量泄露就是这么躲过第一轮清洗的：reToken 只认 8 个已知前缀，裸令牌原样躺着，
// 而当时的谓词也认不出它，于是"洗完报告"写着无命中。
//
// 硬约束（改动前请先读这段）：本包**不得 import**
//   - github.com/NouSart/sump/memory/redact
//   - github.com/NouSart/sump/memory/shared
//   - github.com/NouSart/sump/memory/repair
//
// 第二把尺的价值全在于"独立"：共享谓词 = 同一把尺量两遍。下面的密钥上下文正则
// 与 redact 里那条形状相同但**是手抄的第二份**，抄这件事就是设计本身，不是待清理的重复。
// 测试不许图省事把两把尺接到一起。
//
// 本包只读不写：没有 UPDATE、没有事务、拿不到 *sql.Tx。清洗的活归 repair。
//
// 四路探针各自的强弱，说清楚免得把它当成银弹：
//   - 已知凭据：手里有真值时对**这一份**凭据最强，也最脆——换了机器、轮换过令牌就归零。
//   - 密钥上下文：认"关键词 + 值"，全库校准命中 1 行（强制要分隔符的严格版命中 0，
//     所以分隔符可选）。裸写、前后没有任何关键词的令牌仍然认不出。
//   - 高熵串：不看关键词，看统计。随机凭据一般过阈；真实经验的中文短语不会。
//     16 字符下限配 3.9 bit/字符，是为了躲开重复串、uuid 分段与经验里常见的驼峰标识符
//     （实测 19 位的 `WorkflowExpandPanel` 形状只有 3.83，不过阈）。
//     说清楚代价：**长蛇形英文短语会误报**（`the_quickest_brown_fox_jumps` 实测 4.16，过阈），
//     这条不是被阈值躲开了，是这一路的固有代价；钉在 TestEntropyGateRejectsLowEntropyRuns 里。
//   - 长数字：10~11 位数字串，QQ 号 / 手机号 / Unix 秒的形状。误报是设计内代价，
//     报出来由人读。
//
// 覆盖边界也要说明：只扫正文列，标识列（conversation_id / task_id / source / entry）
// 由 repair 的结构谓词管——那把尺读得懂"收口形状"，本包故意不认摘要，两把尺各查一半。
// 表与列的清单同样是一份手抄的副本（防"清洗没洗到的位置，复检压根没看"）；
// 抄的两份会不会漂，由 repair 包里的 scope 对照测试钉住。
package audit

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

// 手抄的形状（与 redact 各说各话，见包注释）。
var (
	// reCtxSecret 认"令牌 / token / secret / password / api_key / 密码 / 密钥"后面跟的值。
	reCtxSecret = regexp.MustCompile(`(?i)(?:令牌|token|secret|password|api[-_]?key|密码|密钥)\s*[:：=]?\s*[A-Za-z0-9_\-]{8,}`)
	// reRun 取连续的"凭据字母表"片段，供熵判断与摘要比对用。
	reRun = regexp.MustCompile(`[A-Za-z0-9_\-]{16,}`)
	// reKnownRun 同上，下限压到 8 位：已知凭据比对要覆盖短一些的真值。
	reKnownRun = regexp.MustCompile(`[A-Za-z0-9_\-]{8,}`)
	// reLongDigits 10~11 位数字串：QQ 号、手机号、Unix 秒都落在这里。
	reLongDigits = regexp.MustCompile(`\b\d{10,11}\b`)
)

// minEntropyBits 是"这一串像随机凭据"的字符香农熵下限。
const minEntropyBits = 3.9

// minKnownLen 是已知凭据探针接受的最短叶子值：短于它的值多半是配置项名字或布尔，
// 拿去做子串匹配会到处冒红，尺子就废了。
const minKnownLen = 8

// 探针名字，出现在输出里。
const (
	ProbeKnown     = "已知凭据"
	ProbeContext   = "密钥上下文"
	ProbeEntropy   = "高熵串"
	ProbeLongDigit = "长数字"
)

// Column 是被扫描的一处位置（表 + 列）。
type Column struct {
	Table  string
	Name   string
	RowKey string // 定位一行的键列名（正文表用 rowid）
}

// scope 是本包的清单：与 repair 洗的那 5 张表的正文列对齐，独立抄一份。
var scope = []Column{
	{"memories", "content", "rowid"},
	{"memories", "kind", "rowid"},
	{"mem_entries", "content", "rowid"},
	{"mem_nodes", "anchor", "rowid"},
	{"shared_sessions", "title", "rowid"},
	{"shared_tasks", "title", "rowid"},
	{"shared_tasks", "goal", "rowid"},
	{"shared_tasks", "outcome", "rowid"},
	{"shared_tasks", "capabilities", "rowid"},
}

// Scope 暴露清单，供 repair 包的对照测试钉住"两份手抄有没有漂"。
func Scope() []Column {
	out := make([]Column, len(scope))
	copy(out, scope)
	return out
}

// Finding 是一处命中。只带位置与类别，**不带原值**——
// 把凭据从库里搬到 stdout / 日志里不算修复，算换个地方泄露。
type Finding struct {
	Table  string
	RowKey int64
	Column string
	Probe  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s.%d.%s(%s)", f.Table, f.RowKey, f.Column, f.Probe)
}

// KnownValues 从 JSON 文件里递归取出所有字符串叶子，只保留长度够的那些，并顺带数出
// 被跳过的非字符串叶子（数字 / 布尔 / null）。
// 读的是 data/qq-secret.json 这类**本机**配置文件：它不是共享库，允许有真值。
//
// 为什么要报 skipped：本包 writer 那一头（src/service/secrets.go 的 map[string]string）
// 只会写字符串，所以真凭据不会是数字；但 -known 指到谁是由人决定的，手写的配置里
// 完全可能把凭据写成 1234567890 这种不带引号的数。静默丢掉就等于假装看过了——
// data/qq-secret.json 里就有一个 int 叶子（version），这不是假想情形。
func KnownValues(path string) ([]string, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读已知凭据文件 %s 失败: %w", path, err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, 0, fmt.Errorf("已知凭据文件 %s 不是合法 JSON: %w", path, err)
	}
	var out []string
	skipped := 0
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case string:
			if s := strings.TrimSpace(t); len(s) >= minKnownLen {
				out = append(out, s)
			}
		case nil, bool, float64:
			skipped++
		}
	}
	walk(root)
	sort.Strings(out)
	// 去重：同一个值在 JSON 里写两遍，报"取到 N 个"就是虚的（命中数不受影响，
	// probes 每一格最多报一次这一路）。
	return dedupeSorted(out), skipped, nil
}

// dedupeSorted 在已排序的切片上原地去重。
func dedupeSorted(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// Scan 扫库：known 是已知凭据原文（可为空，空则跳过第一路探针）。
// 返回按位置排序的命中；命中数 >0 由调用方决定怎么退出。
func Scan(db *sql.DB, known []string) ([]Finding, error) {
	var bad []Finding
	for _, col := range scope {
		hits, err := scanColumn(db, col, known)
		if err != nil {
			return bad, fmt.Errorf("扫描 %s.%s 失败: %w", col.Table, col.Name, err)
		}
		bad = append(bad, hits...)
	}
	sort.Slice(bad, func(i, j int) bool {
		if bad[i].Table != bad[j].Table {
			return bad[i].Table < bad[j].Table
		}
		if bad[i].RowKey != bad[j].RowKey {
			return bad[i].RowKey < bad[j].RowKey
		}
		if bad[i].Column != bad[j].Column {
			return bad[i].Column < bad[j].Column
		}
		return bad[i].Probe < bad[j].Probe
	})
	return bad, nil
}

func scanColumn(db *sql.DB, col Column, known []string) ([]Finding, error) {
	rows, err := db.Query(`SELECT ` + col.RowKey + `, ` + col.Name + ` FROM ` + col.Table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []Finding
	for rows.Next() {
		var rowKey int64
		var cell any
		if err := rows.Scan(&rowKey, &cell); err != nil {
			return hits, err
		}
		text := asString(cell)
		if text == "" {
			continue
		}
		for _, probe := range probes(text, known) {
			hits = append(hits, Finding{Table: col.Table, RowKey: rowKey, Column: col.Name, Probe: probe})
		}
	}
	return hits, rows.Err()
}

// probes 返回这一格命中的类别（同一格可以多类同时命中，分开报）。
func probes(text string, known []string) []string {
	var out []string
	if len(known) > 0 && hitsKnown(text, known) {
		out = append(out, ProbeKnown)
	}
	if reCtxSecret.MatchString(text) {
		out = append(out, ProbeContext)
	}
	if hasHighEntropy(text) {
		out = append(out, ProbeEntropy)
	}
	if reLongDigits.MatchString(text) {
		out = append(out, ProbeLongDigit)
	}
	return out
}

// hitsKnown 两条路：原文子串包含，以及"某个连续片段的 sha256 前 8 位等于已知值的
// sha256 前 8 位"。
//
// 要写准它们的强弱：在"凭据本身就是一段连续字母数字"这个常见形态下，两条路重合，
// 摘要并不比子串多认出什么（用例 TestKnownDigestPathAgreesWithSubstring 把这一重合钉住）。
// 保留摘要这一路是因为定位 memories rowid 5 用的正是它——把发现泄露的手段从尺子上删掉、
// 只留一个更简洁的实现，是拿可读性换掉可追溯性。
// 真正的盲区在两条路之外：值被拆断、被改写、被换大小写时，两边都够不着。
func hitsKnown(text string, known []string) bool {
	for _, k := range known {
		if strings.Contains(text, k) {
			return true
		}
	}
	targets := make(map[string]bool, len(known))
	for _, k := range known {
		targets[digest8(k)] = true
	}
	for _, run := range reKnownRun.FindAllString(text, -1) {
		if targets[digest8(run)] {
			return true
		}
	}
	return false
}

// hasHighEntropy 任一段 16 位以上的字母数字串，字符熵过阈就算命中。
func hasHighEntropy(text string) bool {
	for _, run := range reRun.FindAllString(text, -1) {
		if entropyOf(run) >= minEntropyBits {
			return true
		}
	}
	return false
}

// entropyOf 按字节算香农熵（输入已经被探针限定成 ASCII 字母数字段）。
func entropyOf(s string) float64 {
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	total := float64(len(s))
	var h float64
	for _, n := range freq {
		if n == 0 {
			continue
		}
		p := float64(n) / total
		h -= p * math.Log2(p)
	}
	return h
}

func digest8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

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
