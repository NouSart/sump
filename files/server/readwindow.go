// readwindow.go：files.read 的统一字符窗口。
//
// 分页必须发生在“已经按格式拼好的可见文本”上，否则每种格式都会各写一套偏移，
// 续读拼回来就少字/多字。窗口同时统计完整投影长度（total），只保留 [offset,offset+limit)
// 那一段在内存里——格式层可以照常逐行 / 逐页喂，不必先拼完整篇。
package server

import "strings"

type readWindow struct {
	offset  int
	limit   int
	total   int // 已消费的完整投影字符数（rune）
	emitted int // 已写入返回文本的字符数（rune）
	started bool
	b       strings.Builder
}

func newReadWindow(offset, limit int) *readWindow {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	return &readWindow{offset: offset, limit: limit}
}

// add 消费一段完整投影文本：窗口之外只累计 total，窗口之内写入返回文本。
func (w *readWindow) add(s string) {
	rs := []rune(s)
	n := len(rs)
	if n == 0 {
		return
	}
	start, end := w.total, w.total+n
	w.total = end
	if end <= w.offset || w.emitted >= w.limit {
		return
	}
	from := 0
	if w.offset > start {
		from = w.offset - start
	}
	room := w.limit - w.emitted
	if room > n-from {
		room = n - from
	}
	if room <= 0 {
		return
	}
	w.b.WriteString(string(rs[from : from+room]))
	w.emitted += room
}

// line 按行追加：行间补一个换行，末行不补——与历史 read 输出（无尾部换行）一致。
//
// 从开头读（offset=0）时按整行停：下一行连同分隔换行放不进剩余额度，就整行不要，
// 避免把“第二段”切成“第二”。续读（offset>0）必须按字符精确切片——否则从块中间
// 接上时，后续整块会被误当作“放不下”而整块丢掉，拼回来就少字。
func (w *readWindow) line(s string) {
	prefix := ""
	if w.started {
		prefix = "\n"
	}
	full := prefix + s
	remaining := w.limit - w.emitted
	if w.offset == 0 && w.emitted > 0 && remaining < len([]rune(full)) {
		w.total += len([]rune(full))
		w.started = true
		return
	}
	w.add(full)
	w.started = true
}

// chunkLine 与 line 相同地维护行分隔，但允许在行内按字符截断。
// PDF 的页正文本来就是连续文本，历史行为也是“页内按字符截断”；续读坐标因此更细。
func (w *readWindow) chunkLine(s string) {
	if w.started {
		w.add("\n")
	}
	w.add(s)
	w.started = true
}

func (w *readWindow) text() string { return w.b.String() }

// next 返回续读起点；0 表示已经到末尾。
func (w *readWindow) next() int {
	if w.offset+w.emitted >= w.total {
		return 0
	}
	return w.offset + w.emitted
}

// truncated 表示这次返回没覆盖完整投影（有下一块，或格式层另有页数上限）。
func (w *readWindow) truncated() bool { return w.next() != 0 }
