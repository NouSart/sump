// 检索（v0）：bigram（相邻二字）近似分词 + 子串打分——纯函数，不碰库。
// 中文没有空格：把“相邻两字组合”当作词的近似——查询与条目重叠越多越相关。
// 实测 FTS5 trigram 对 1~2 字词全部打空，故不用它；升级路径：疼了换 FTS5 预索引或 embedding 语义召回。
package longterm

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Terms 提取查询词：汉字串 → 所有相邻二字组合（孤立单字 → 该字）；字母/数字 → 整词（小写）。
// 结果去重且保持出现顺序。
func Terms(text string) []string {
	var out []string
	seen := map[string]bool{}
	emit := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}

	var han, word []rune
	flushHan := func() {
		switch len(han) {
		case 0:
		case 1:
			emit(string(han))
		default:
			for i := 0; i+1 < len(han); i++ {
				emit(string(han[i : i+2]))
			}
		}
		han = han[:0]
	}
	flushWord := func() {
		if len(word) >= 2 {
			emit(strings.ToLower(string(word)))
		}
		word = word[:0]
	}

	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return out
}

// Score 返回条目内容命中的查询词数。
func Score(terms []string, content string) int {
	lower := strings.ToLower(content)
	n := 0
	for _, t := range terms {
		if strings.Contains(lower, t) {
			n++
		}
	}
	return n
}

// Rank 按命中词数排序（只保留 >0 分），同分按 priority 大、新近优先；
// 受条数与字符预算（按条目正文字符数计）约束。
func Rank(entries []Entry, query string, limit, maxChars int) []Entry {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		e Entry
		s int
	}
	var hits []scored
	for _, e := range entries {
		if s := Score(terms, e.Content); s > 0 {
			hits = append(hits, scored{e: e, s: s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.s != b.s {
			return a.s > b.s
		}
		if a.e.Priority != b.e.Priority {
			return a.e.Priority > b.e.Priority
		}
		return a.e.ID > b.e.ID
	})

	var out []Entry
	used := 0
	for _, h := range hits {
		if limit > 0 && len(out) >= limit {
			break
		}
		cost := len([]rune(h.e.Content))
		if maxChars > 0 && used+cost > maxChars {
			continue
		}
		used += cost
		out = append(out, h.e)
	}
	return out
}

// Core 取核心条目（priority>0）：按 priority 降序、新近优先；受条数与字符预算约束。
func Core(entries []Entry, limit, maxChars int) []Entry {
	var core []Entry
	for _, e := range entries {
		if e.Priority > 0 {
			core = append(core, e)
		}
	}
	sort.SliceStable(core, func(i, j int) bool {
		if core[i].Priority != core[j].Priority {
			return core[i].Priority > core[j].Priority
		}
		return core[i].ID > core[j].ID
	})

	var out []Entry
	used := 0
	for _, e := range core {
		if limit > 0 && len(out) >= limit {
			break
		}
		cost := len([]rune(e.Content))
		if maxChars > 0 && used+cost > maxChars {
			continue
		}
		used += cost
		out = append(out, e)
	}
	return out
}

// RankSemantic 按查询向量与条目向量的余弦相似度排序（语义召回）：
// 只保留 score ≥ minScore 的条目（无向量 / 维度不符的条目跳过）；
// 同分按 priority 大、新近（id 大）优先；受条数与字符预算约束（与 Rank 一致）。
func RankSemantic(entries []Entry, queryVec []float32, limit, maxChars int, minScore float32) []Entry {
	if len(queryVec) == 0 {
		return nil
	}
	type scored struct {
		e Entry
		s float32
	}
	var hits []scored
	for _, e := range entries {
		if len(e.Embedding) != len(queryVec) {
			continue
		}
		s := cosine(queryVec, e.Embedding)
		if s < minScore {
			continue
		}
		hits = append(hits, scored{e: e, s: s})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.s != b.s {
			return a.s > b.s
		}
		if a.e.Priority != b.e.Priority {
			return a.e.Priority > b.e.Priority
		}
		return a.e.ID > b.e.ID
	})

	var out []Entry
	used := 0
	for _, h := range hits {
		if limit > 0 && len(out) >= limit {
			break
		}
		cost := len([]rune(h.e.Content))
		if maxChars > 0 && used+cost > maxChars {
			continue
		}
		used += cost
		out = append(out, h.e)
	}
	return out
}

// cosine 两个等长向量的余弦相似度（调用方通常已归一化；仍做规范化兜底）。
func cosine(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(na*nb))
}
