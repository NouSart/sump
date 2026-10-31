// PDF 支持：read 按页提取纯文本，edit_pdf 做元数据与页操作（旋转 / 删页 / 保留页）。
// 读取基于 ledongthuc/pdf（rsc/pdf 的活跃 fork，纯 Go 无 CGO）；修改基于 pdfcpu（纯 Go 无 CGO）。
// 加密 PDF 明确拒绝；扫描件（图片型 PDF，无文本层）read 报错引导，不做 OCR。
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/NouSart/sump/protocol"
)

// pdfMaxPages 是 read 的页数上限：防「万页 PDF」把读取拖垮（与压缩炸弹防线同思路的预算控制）。
const pdfMaxPages = 2000

// errPdfEncrypted 是加密 PDF 的统一拒绝理由：库的原生报错（英文、且把加密与损坏混为一谈）
// 不适合直接给模型和用户看。
var errPdfEncrypted = errors.New("该 PDF 已加密，暂不支持读取或编辑；请先解密或另存为无密码版本")

// isEncryptedPdf 用 pdfcpu 的哨兵错误判断是否加密，而不是匹配错误字符串。
// 只在解析已经失败时调用，成功路径不付这次额外解析的代价。
func isEncryptedPdf(data []byte) bool {
	_, err := api.PageCount(context.Background(), bytes.NewReader(data), model.NewDefaultConfiguration())
	return errors.Is(err, pdfcpu.ErrEncrypted) || errors.Is(err, pdfcpu.ErrWrongPassword)
}

// isPdfFile 判定 .pdf：mime 为准；扩展名兜底（pdf 嗅探通常即 application/pdf）。
func isPdfFile(mime, filename string) bool {
	mime = cleanMime(mime)
	if mime == "application/pdf" {
		return true
	}
	return strings.EqualFold(filepath.Ext(filename), ".pdf") &&
		(mime == "" || mime == "application/octet-stream")
}

// pdfToText 把 PDF 按页提取纯文本，每页以 "# 第 N 页" 分节（页边界截断，单页内按字符截断）。
// pageLimit 是页数上限：超出部分不读并置 truncated。空页（无文本层）跳过，若全篇无文本则报错。
func pdfToText(data []byte, maxChars, pageLimit int) (string, bool, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// 加密的 PDF 在读取库里报成「malformed PDF: … encryption key」，
		// 与真正的损坏混在一起；这里分开说清楚，用户才知道下一步该做什么。
		if isEncryptedPdf(data) {
			return "", false, errPdfEncrypted
		}
		return "", false, fmt.Errorf("解析 PDF 失败（文件可能已损坏）: %w", err)
	}
	total := r.NumPage()
	if total <= 0 {
		return "", false, errors.New("该 PDF 无页面，无法读取")
	}
	limit := total
	if limit > pageLimit {
		limit = pageLimit
	}

	var b strings.Builder
	used := 0
	truncated := false
	writeBlock := func(s string) bool {
		n := utf8.RuneCountInString(s)
		if used+n > maxChars {
			return false
		}
		b.WriteString(s)
		used += n
		return true
	}
	for i := 1; i <= limit; i++ {
		pageText, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			return "", false, fmt.Errorf("第 %d 页文本提取失败: %w", i, err)
		}
		body := strings.TrimSpace(pageText)
		if body == "" {
			continue
		}
		if !writeBlock(fmt.Sprintf("# 第 %d 页\n", i)) {
			truncated = true
			break
		}
		if !writeBlock(body + "\n") {
			if remain := maxChars - used; remain > 0 {
				part, _ := truncateRunes(body, remain)
				b.WriteString(part)
			}
			truncated = true
			break
		}
	}
	if total > pageLimit {
		truncated = true
	}
	if b.Len() == 0 {
		return "", false, errors.New("未提取到文本，可能是扫描件（图片型 PDF），暂不支持 OCR")
	}
	return strings.TrimRight(b.String(), "\n"), truncated, nil
}

// ---------- edit_pdf：op 注册表扩展接缝 ----------

// pdfOpHandler 是单个编辑操作的处理函数：就地处理 data 并返回新字节。
// 新增 op 类型只需实现一个 handler 并注册进 pdfOpHandlers，动作协议无需改动（与 edit_ppt 同口径）。
type pdfOpHandler func(data []byte, op protocol.PdfEditOperation, conf *model.Configuration) ([]byte, error)

var pdfOpHandlers = map[string]pdfOpHandler{
	"set_info":     pdfOpSetInfo,
	"rotate":       pdfOpRotate,
	"delete_pages": pdfOpDeletePages,
	"keep_pages":   pdfOpKeepPages,
}

// editPdf 对 PDF 依次执行操作列表，返回新字节和生效操作数。
// 所有操作成功后才返回（任一失败整体报错、不落盘），保证原子性；op 作用于前一步结果，
// 因此页选集按「当前页数」解析。
func editPdf(data []byte, ops []protocol.PdfEditOperation) ([]byte, int, error) {
	conf := model.NewDefaultConfiguration() // 默认已 ValidationRelaxed：容忍真实世界 PDF 的轻微不规范
	cur := data
	for i, op := range ops {
		handler, ok := pdfOpHandlers[op.Op]
		if !ok {
			return nil, 0, fmt.Errorf("第 %d 个操作不支持 op=%q（v0 支持 set_info/rotate/delete_pages/keep_pages）", i+1, op.Op)
		}
		out, err := handler(cur, op, conf)
		if err != nil {
			// pdfcpu 对加密文件抛的是英文库错误（"please provide the correct password"），
			// 直接转给模型只会得到一句看不懂的话。
			if errors.Is(err, pdfcpu.ErrEncrypted) || errors.Is(err, pdfcpu.ErrWrongPassword) {
				return nil, 0, errPdfEncrypted
			}
			return nil, 0, fmt.Errorf("第 %d 个操作 %s 失败: %w", i+1, op.Op, err)
		}
		cur = out
	}
	return cur, len(ops), nil
}

// runPdfOp 以内存流方式跑一个 pdfcpu 操作，返回新字节。
func runPdfOp(data []byte, conf *model.Configuration, fn func(rs io.ReadSeeker, w io.Writer) error) ([]byte, error) {
	var out bytes.Buffer
	if err := fn(bytes.NewReader(data), &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pdfSelectedPages 解析页选集；空串返回 nil（表示全部页）。越界 / 格式错给出中文报错。
func pdfSelectedPages(data []byte, sel string, conf *model.Configuration) ([]string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, nil
	}
	n, err := api.PageCount(context.Background(), bytes.NewReader(data), conf)
	if err != nil {
		return nil, fmt.Errorf("读取 PDF 页数失败: %w", err)
	}
	return parsePageSelection(sel, n)
}

func pdfOpSetInfo(data []byte, op protocol.PdfEditOperation, conf *model.Configuration) ([]byte, error) {
	if len(op.Properties) == 0 {
		return nil, errors.New("set_info 需要 properties（至少一个键值对）")
	}
	return runPdfOp(data, conf, func(rs io.ReadSeeker, w io.Writer) error {
		return api.AddProperties(context.Background(), rs, w, op.Properties, conf)
	})
}

func pdfOpRotate(data []byte, op protocol.PdfEditOperation, conf *model.Configuration) ([]byte, error) {
	d := ((op.Degrees % 360) + 360) % 360
	if d == 0 || d%90 != 0 {
		return nil, errors.New("rotate 需要 90 的整数倍角度（90/180/270）")
	}
	pages, err := pdfSelectedPages(data, op.Pages, conf)
	if err != nil {
		return nil, err
	}
	return runPdfOp(data, conf, func(rs io.ReadSeeker, w io.Writer) error {
		return api.Rotate(context.Background(), rs, w, d, pages, conf)
	})
}

func pdfOpDeletePages(data []byte, op protocol.PdfEditOperation, conf *model.Configuration) ([]byte, error) {
	if strings.TrimSpace(op.Pages) == "" {
		return nil, errors.New("delete_pages 需要 pages（不能为空）")
	}
	pages, err := pdfSelectedPages(data, op.Pages, conf)
	if err != nil {
		return nil, err
	}
	return runPdfOp(data, conf, func(rs io.ReadSeeker, w io.Writer) error {
		return api.RemovePages(context.Background(), rs, w, pages, conf)
	})
}

func pdfOpKeepPages(data []byte, op protocol.PdfEditOperation, conf *model.Configuration) ([]byte, error) {
	if strings.TrimSpace(op.Pages) == "" {
		return nil, errors.New("keep_pages 需要 pages（不能为空）")
	}
	pages, err := pdfSelectedPages(data, op.Pages, conf)
	if err != nil {
		return nil, err
	}
	return runPdfOp(data, conf, func(rs io.ReadSeeker, w io.Writer) error {
		return api.Trim(context.Background(), rs, w, pages, conf)
	})
}

// parsePageSelection 把 "1-3,5" 解析为去重、升序的页码列表（pdfcpu 页集格式），校验 1..total。
func parsePageSelection(sel string, total int) ([]string, error) {
	seen := make(map[int]bool)
	for _, part := range strings.Split(sel, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, err := parsePageRange(part, total)
		if err != nil {
			return nil, err
		}
		for p := from; p <= to; p++ {
			seen[p] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("页选集 %q 无效", sel)
	}
	pages := make([]int, 0, len(seen))
	for p := range seen {
		pages = append(pages, p)
	}
	sort.Ints(pages)
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = strconv.Itoa(p)
	}
	return out, nil
}

// parsePageRange 解析单个页号或区间 "M-N"，校验 1..total 且升序。
func parsePageRange(part string, total int) (int, int, error) {
	if lo, hi, ok := strings.Cut(part, "-"); ok {
		from, err1 := strconv.Atoi(strings.TrimSpace(lo))
		to, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || from < 1 || to > total || from > to {
			return 0, 0, fmt.Errorf("页选集 %q 无效（页码需在 1-%d，区间升序）", part, total)
		}
		return from, to, nil
	}
	p, err := strconv.Atoi(part)
	if err != nil || p < 1 || p > total {
		return 0, 0, fmt.Errorf("页选集 %q 无效（页码需在 1-%d）", part, total)
	}
	return p, p, nil
}
