// files 服务的核心：把文件动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包）。
// save：从 URL 或 base64 保存文件；fetch：把文件转成 base64 发回调用方；
// read：读取文本类文件的内容（超长按 read.max_chars 截断）——声明 tool:true，
// 经名册自动成为 LLM 工具（files__read），模型凭占位文本里的 id 自取。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"

	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// Config 是 files 服务的启动配置。
type Config struct {
	Name              string        // 服务名（注册用，默认 files）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	Store             *store.Store  // 文件库
	MaxBytes          int64         // 单文件上限（默认 100MiB；作为声明预设上报）
	ReadMaxChars      int           // read 返回的最大字符数（默认 4000；作为声明预设上报）
	DBPath            string        // 元数据库路径（只读项展示用：换它要重启）
	Dir               string        // 文件二进制目录（只读项展示用：换它要重启）
}

// Server 是 files 服务实例（服务骨架 + 文件库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu        sync.Mutex // 运行时可设项（设置下发 / 拉取即时生效）
	maxBytes     int64      // 当前单文件上限（字节）
	readMaxChars int        // 当前 read 截断阈值（字符数）

	writeMu sync.Mutex // write 的 read-modify-write 串行化（防并发改同一文件丢更新）
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.MaxBytes < 1<<20 {
		cfg.MaxBytes = 1 << 20 // 声明以 MB 计——小于 1MiB 的引导值归一到 1MiB
	}
	if cfg.ReadMaxChars <= 0 {
		cfg.ReadMaxChars = 4000
	}
	s := &Server{cfg: cfg, logger: logger, maxBytes: cfg.MaxBytes, readMaxChars: cfg.ReadMaxChars}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "文件",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ActionTimeout:     60 * time.Second, // save 含下载，放宽动作超时
		Description:       "文件服务：保存文件（URL/base64），按需取回（fetch）或读取文本内容（read：文本类/.xlsx/.docx/.pptx/.pdf）",
		Provides: []protocol.Provide{
			{Action: "save", Tool: true, Input: "{url} 或 {data(base64), filename, mime}", Output: "{id, filename, mime, size}"},
			{Action: "fetch", Input: "{id}", Output: "{id, filename, mime, data(base64), size}"},
			{Action: "read", Input: "{id}", Output: "{id, filename, text, truncated}（文本类 / .xlsx 转 TSV / .docx/.pptx/.pdf 提取正文；其余二进制报错）", Tool: true},
			{Action: "write", Input: "{id, sheet, edits:[{cell, value}]}", Output: "{id, sheet, updated}（就地修改 .xlsx；先 read 看清坐标再改）", Tool: true},
			{Action: "edit_word", Input: "{id, operations:[{op, old, new}]}", Output: "{id, replaced}（就地修改 .docx 正文；v0 仅 op='replace'）", Tool: true},
			{Action: "edit_ppt", Input: "{id, operations:[{op, old, new}]}", Output: "{id, replaced}（就地修改 .pptx 幻灯片文本；v0 仅 op='replace'）", Tool: true},
			{Action: "edit_pdf", Input: "{id, operations:[{op, pages?, degrees?, properties?}]}", Output: "{id, applied}（就地修改 .pdf；v0 支持 set_info/rotate/delete_pages/keep_pages）", Tool: true},
			{Action: "outline", Input: "{id, depth?}", Output: "{id, doc_kind, digest, nodes:[{id, type, text, level, truncated, children}], node_count, chars, tables, parts, warnings}（先看结构再决定读哪段；digest 是整篇指纹）", Tool: true},
			{Action: "read_range", Input: "{id, node?, offset?, limit?}", Output: "{id, doc_kind, digest, node, text, total, next}（按节点分页读；next 非 0 表示还有下一块）", Tool: true},
			{Action: "find", Input: "{id, text, node?, regex?, limit?}", Output: "{id, digest, matches:[{node, run, start, end, context}]}（只找位置不改文档；同段多处各占一条）", Tool: true},
			{Action: "patch", Input: "{id, anchor?, operations:[{op, node, range?, text?, anchor?}]}", Output: "{id, applied, digest, diff, warnings}（按节点地址就地改 .docx；op = replace_text / delete_node；整批原子）", Tool: true},
			{Action: "verify", Input: "{id, digest?, contains?, not_contains?}", Output: "{id, ok, digest, failures}（改完自查：指纹与片段核对）", Tool: true},
		},
		Logger: logger,
	})
	// 可设置项：预设 = 本服务的启动引导值（来源由服务自己决定）。
	s.HandleSetting("max_mb", strconv.FormatInt(cfg.MaxBytes>>20, 10), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数（MB），收到 %q", v)
		}
		s.setMu.Lock()
		s.maxBytes = n << 20
		s.setMu.Unlock()
		return nil
	}, service.Label("单个文件大小上限"),
		service.Hint("超过这个大小的文件会被拒绝写入或读取"),
		service.As("number"),
		service.Unit("MB"))
	s.HandleSetting("read_max_chars", strconv.Itoa(cfg.ReadMaxChars), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数（字符数），收到 %q", v)
		}
		s.setMu.Lock()
		s.readMaxChars = int(n)
		s.setMu.Unlock()
		return nil
	}, service.Label("单次读取的字数上限"),
		service.Hint("读文件时最多往回带多少字，超出的会截断——调大能少丢上下文，但更费模型额度"),
		service.As("number"),
		service.Unit("chars"))
	s.Handle("save", s.actionSave)
	s.Handle("fetch", s.actionFetch)
	s.Handle("read", s.actionRead)
	s.Handle("write", s.actionWrite)
	s.Handle("edit_word", s.actionEditWord)
	s.Handle("edit_ppt", s.actionEditPpt)
	s.Handle("edit_pdf", s.actionEditPdf)
	s.Handle("outline", s.actionDocOutline)
	s.Handle("read_range", s.actionDocReadRange)
	s.Handle("find", s.actionDocFind)
	s.Handle("patch", s.actionDocPatch)
	s.Handle("verify", s.actionDocVerify)
	// 只读项：启动参数带起来的（换它要重启）。db/dir 是存储身份——运行中换库/换目录
	// 意味着一批在途读写换个落点，不做（与 images 同口径）。
	s.HandleReadOnly("db", cfg.DBPath)
	s.HandleReadOnly("dir", cfg.Dir)
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	return s
}

// maxBytesNow 返回当前单文件上限（字节）。
func (s *Server) maxBytesNow() int64 {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxBytes
}

// readMaxCharsNow 返回当前 read 截断阈值（字符数）。
func (s *Server) readMaxCharsNow() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.readMaxChars
}

// maxExpandBytes 是**解压后**的字节上限（压缩炸弹防线）：.xlsx/.docx 都是 zip，
// 文件本体受 max_mb 管，但解出来的大小不受它管——一个 1MiB 的包可以解出 GB 级内容。
// 定值 256MiB：文本类文档解到这个量级已远超 read 的截断阈值（默认 4000 字符），
// 真需要读更大的表，说明该走"拆文件"而不是把服务的内存当桶（宁可不给旋钮，也不给一个会 OOM 的旋钮）。
//
// 是变量而非常量：测试会临时调小它来验这道防线，不必造 256MiB 的夹具（生产代码里只读）。
var maxExpandBytes int64 = 256 << 20

// expandError 把解压超限的错误统一成一句人话（含上限值，便于排查）。
func expandError(err error) error {
	return fmt.Errorf("解压后内容超过 %d 字节上限（压缩炸弹防线）: %w", maxExpandBytes, err)
}

// ---------- 动作 ----------

// actionSave 保存一个文件：URL 或 base64 二选一，filename 必填。
func (s *Server) actionSave(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FileSavePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	var data []byte
	var mimeHint string
	var err error
	limit := s.maxBytesNow()
	switch {
	case strings.TrimSpace(p.URL) != "":
		data, mimeHint, err = fetchURL(p.URL, limit)
	case strings.TrimSpace(p.Data) != "":
		data, err = base64.StdEncoding.DecodeString(p.Data)
	default:
		return nil, errors.New("save: url 或 data 必填")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("save: 文件为空")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("save: 文件超过 %d 字节上限", limit)
	}
	filename := strings.TrimSpace(p.Filename)
	if filename == "" && strings.TrimSpace(p.URL) != "" {
		// 只给了 URL 时按 URL 末段取名：模型用 save 抓一个网页/附件时不该被迫先猜文件名。
		if name := filenameFromURL(p.URL); name != "" {
			filename = name
		}
	}
	if filename == "" {
		return nil, errors.New("save: filename 必填（用 URL 保存时可由 URL 末段推断，但 URL 里得有个像文件名的末段）")
	}
	mime := p.Mime
	if mime == "" {
		mime = mimeHint
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	id, err := s.cfg.Store.Save(data, filename, mime)
	if err != nil {
		return nil, fmt.Errorf("保存失败: %w", err)
	}
	s.logger.Printf("已保存文件 %s（%s, %s, %d 字节）", id, filename, mime, len(data))
	return protocol.FileSaveResult{ID: id, Filename: filename, Mime: mime, Size: int64(len(data))}, nil
}

// actionFetch 把文件转成 base64 发回调用方（凭牌取全量）。
func (s *Server) actionFetch(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FileFetchPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("fetch: id 必填")
	}
	data, meta, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	return protocol.FileFetchResult{
		ID:       p.ID,
		Filename: meta.Filename,
		Mime:     meta.Mime,
		Data:     base64.StdEncoding.EncodeToString(data),
		Size:     int64(len(data)),
	}, nil
}

// actionRead 读取文件的可文本化内容（LLM 工具）：先 Stat 判定类型再读本体。
// 文本类直接读取；.xlsx 逐 sheet 转 TSV；.docx 提取正文段落；.xls/.doc 与其余二进制明确拒绝。
// 截断阈值 = read_max_chars（截断置 truncated——“没给全”必须显式告知模型）。
func (s *Server) actionRead(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FileReadPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("read: id 必填")
	}
	meta, err := s.cfg.Store.Stat(p.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case isLegacyXls(meta.Mime, meta.Filename):
		return nil, errors.New("read: 旧版 .xls 不支持，请另存为 .xlsx 后重试")
	case isLegacyDoc(meta.Mime, meta.Filename):
		return nil, errors.New("read: 旧版 .doc 不支持，请另存为 .docx 后重试")
	case isLegacyPpt(meta.Mime, meta.Filename):
		return nil, errors.New("read: 旧版 .ppt 不支持，请另存为 .pptx 后重试")
	case isExcelFile(meta.Mime, meta.Filename):
		return s.readXlsx(p.ID, meta)
	case isWordFile(meta.Mime, meta.Filename):
		return s.readDocx(p.ID, meta)
	case isPptFile(meta.Mime, meta.Filename):
		return s.readPptx(p.ID, meta)
	case isPdfFile(meta.Mime, meta.Filename):
		return s.readPdf(p.ID, meta)
	case isTextual(meta.Mime, meta.Filename):
		return s.readText(p.ID, meta)
	default:
		return nil, fmt.Errorf("read: 该文件类型（%s）暂不支持阅读，仅支持文本类、.xlsx、.docx、.pptx 与 .pdf", meta.Mime)
	}
}

// readText 文本类读取：UTF-8 校验 + 按字符数截断。
func (s *Server) readText(id string, meta store.Meta) (any, error) {
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("read: %s 的内容不是有效的 UTF-8 文本", meta.Filename)
	}
	text, truncated := truncateRunes(string(data), s.readMaxCharsNow())
	s.logger.Printf("已读取文件 %s（%s, %d 字节, 截断=%v）", id, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: id, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
}

// readXlsx .xlsx 读取：逐 sheet 转 TSV（行边界截断）。
func (s *Server) readXlsx(id string, meta store.Meta) (any, error) {
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, err
	}
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit:    maxExpandBytes,
		UnzipXMLSizeLimit: maxExpandBytes,
	})
	if err != nil {
		return nil, expandError(fmt.Errorf("read: 解析 %s 为 xlsx 失败: %w", meta.Filename, err))
	}
	defer f.Close()
	text, truncated := xlsxToText(f, s.readMaxCharsNow())
	s.logger.Printf("已读取 xlsx %s（%s, %d 字节, 截断=%v）", id, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: id, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
}

// readDocx .docx 读取：提取正文段落文本（段落边界截断）。
func (s *Server) readDocx(id string, meta store.Meta) (any, error) {
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, err
	}
	text, truncated, err := docxToText(data, s.readMaxCharsNow(), maxExpandBytes)
	if err != nil {
		return nil, fmt.Errorf("read: 解析 %s 为 docx 失败: %w", meta.Filename, err)
	}
	s.logger.Printf("已读取 docx %s（%s, %d 字节, 截断=%v）", id, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: id, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
}

// readPptx .pptx 读取：按幻灯片提取文本（幻灯片边界截断，单页内按字符截断）。
func (s *Server) readPptx(id string, meta store.Meta) (any, error) {
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, err
	}
	text, truncated, err := pptxToText(data, s.readMaxCharsNow(), maxExpandBytes)
	if err != nil {
		return nil, fmt.Errorf("read: 解析 %s 为 pptx 失败: %w", meta.Filename, err)
	}
	s.logger.Printf("已读取 pptx %s（%s, %d 字节, 截断=%v）", id, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: id, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
}

// readPdf .pdf 读取：按页提取文本（页边界截断，单页内按字符截断；页数受 pdfMaxPages 封顶）。
func (s *Server) readPdf(id string, meta store.Meta) (any, error) {
	data, _, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, err
	}
	text, truncated, err := pdfToText(data, s.readMaxCharsNow(), pdfMaxPages)
	if err != nil {
		return nil, fmt.Errorf("read: 解析 %s 为 pdf 失败: %w", meta.Filename, err)
	}
	s.logger.Printf("已读取 pdf %s（%s, %d 字节, 截断=%v）", id, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: id, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
}

// actionWrite 编辑 xlsx 的单元格（LLM 工具，就地修改权威副本）。
// 校验链：id 存在 → 必须 .xlsx → sheet 存在（报错附可用表清单）→ 坐标全量预检 → 写回库。
// 全局写锁串行化 read-modify-write，防并发改同一文件丢更新。
func (s *Server) actionWrite(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FileWritePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("write: id 必填")
	}
	if strings.TrimSpace(p.Sheet) == "" {
		return nil, errors.New("write: sheet 必填")
	}
	if len(p.Edits) == 0 {
		return nil, errors.New("write: edits 至少一条")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	meta, err := s.cfg.Store.Stat(p.ID)
	if err != nil {
		return nil, err
	}
	if !isExcelFile(meta.Mime, meta.Filename) {
		return nil, fmt.Errorf("write: 仅支持编辑 .xlsx 文件（%s 的类型是 %s）", meta.Filename, meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	out, n, err := editXlsx(data, p.Sheet, p.Edits)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Overwrite(p.ID, out); err != nil {
		return nil, fmt.Errorf("write: 写回库失败: %w", err)
	}
	s.logger.Printf("已编辑 xlsx %s（%s, 表 %s, %d 格）", p.ID, meta.Filename, p.Sheet, n)
	return protocol.FileWriteResult{ID: p.ID, Sheet: p.Sheet, Updated: n}, nil
}

// actionEditWord 编辑 .docx 正文（LLM 工具，就地修改权威副本）。
// 校验链：id 存在 → 必须 .docx → operations 非空 → 预检查所有 old 存在 → 写回库。
func (s *Server) actionEditWord(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FileWordEditPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("edit_word: id 必填")
	}
	if len(p.Operations) == 0 {
		return nil, errors.New("edit_word: operations 至少一条")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	meta, err := s.cfg.Store.Stat(p.ID)
	if err != nil {
		return nil, err
	}
	if !isWordFile(meta.Mime, meta.Filename) {
		return nil, fmt.Errorf("edit_word: 仅支持编辑 .docx 文件（%s 的类型是 %s）", meta.Filename, meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	out, n, err := editDocx(data, p.Operations, maxExpandBytes)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Overwrite(p.ID, out); err != nil {
		return nil, fmt.Errorf("edit_word: 写回库失败: %w", err)
	}
	s.logger.Printf("已编辑 docx %s（%s, %d 处）", p.ID, meta.Filename, n)
	return protocol.FileWordEditResult{ID: p.ID, Replaced: n}, nil
}

// actionEditPpt 编辑 .pptx 幻灯片文本（LLM 工具，就地修改权威副本）。
// 校验链：id 存在 → 必须 .pptx → operations 非空 → 预检查所有 old 存在 → 写回库。
func (s *Server) actionEditPpt(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FilePptEditPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("edit_ppt: id 必填")
	}
	if len(p.Operations) == 0 {
		return nil, errors.New("edit_ppt: operations 至少一条")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	meta, err := s.cfg.Store.Stat(p.ID)
	if err != nil {
		return nil, err
	}
	if !isPptFile(meta.Mime, meta.Filename) {
		return nil, fmt.Errorf("edit_ppt: 仅支持编辑 .pptx 文件（%s 的类型是 %s）", meta.Filename, meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	out, n, err := editPptx(data, p.Operations, maxExpandBytes)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Overwrite(p.ID, out); err != nil {
		return nil, fmt.Errorf("edit_ppt: 写回库失败: %w", err)
	}
	s.logger.Printf("已编辑 pptx %s（%s, %d 处）", p.ID, meta.Filename, n)
	return protocol.FilePptEditResult{ID: p.ID, Replaced: n}, nil
}

// actionEditPdf 编辑 .pdf（LLM 工具，就地修改权威副本）。
// 校验链：id 存在 → 必须 .pdf → operations 非空 → 依次应用（原子）→ 写回库。
func (s *Server) actionEditPdf(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.FilePdfEditPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("edit_pdf: id 必填")
	}
	if len(p.Operations) == 0 {
		return nil, errors.New("edit_pdf: operations 至少一条")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	meta, err := s.cfg.Store.Stat(p.ID)
	if err != nil {
		return nil, err
	}
	if !isPdfFile(meta.Mime, meta.Filename) {
		return nil, fmt.Errorf("edit_pdf: 仅支持编辑 .pdf 文件（%s 的类型是 %s）", meta.Filename, meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	out, n, err := editPdf(data, p.Operations)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Overwrite(p.ID, out); err != nil {
		return nil, fmt.Errorf("edit_pdf: 写回库失败: %w", err)
	}
	s.logger.Printf("已编辑 pdf %s（%s, %d 个操作）", p.ID, meta.Filename, n)
	return protocol.FilePdfEditResult{ID: p.ID, Applied: n}, nil
}

// ---------- 文本类判定与截断 ----------

// textMIMETypes 是 text/ 前缀之外可按文本阅读的精确 MIME 白名单。
var textMIMETypes = map[string]bool{
	"application/json":       true,
	"application/xml":        true,
	"application/javascript": true,
	"application/x-yaml":     true,
	"application/yaml":       true,
	"application/toml":       true,
	"application/x-sh":       true,
	"application/sql":        true,
	"image/svg+xml":          true,
}

// textExts 是 MIME 不可靠（application/octet-stream 或为空）时按扩展名的兜底白名单。
var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".log": true, ".csv": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".xml": true,
	".ini": true, ".cfg": true, ".conf": true, ".env": true, ".editorconfig": true,
	".sh": true, ".bat": true, ".ps1": true, ".gitignore": true, ".gitattributes": true,
	".py": true, ".go": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
	".c": true, ".h": true, ".cpp": true, ".hpp": true, ".java": true, ".rs": true,
	".sql": true, ".html": true, ".htm": true, ".css": true, ".svg": true,
}

// isTextual 判定一个文件是否可按文本阅读：mime 优先，扩展名兜底。
func isTextual(mime, filename string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch {
	case strings.HasPrefix(mime, "text/"):
		return true
	case textMIMETypes[mime]:
		return true
	case mime == "" || mime == "application/octet-stream":
		return textExts[strings.ToLower(filepath.Ext(filename))]
	}
	return false
}

// truncateRunes 按字符数截断（不按字节，避免切断 UTF-8 序列）；未超长原样返回。
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// fetchURL 下载文件：限时 30s、限制大小（多读 1 字节探测超限）。
// filenameFromURL 从 URL 末段推断文件名（去掉查询串、解码转义）。
// 不像文件名（末段为空 / 含路径分隔符或控制字符 / 过长）时返回空串——由调用方继续要求显式 filename。
func filenameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	if strings.ContainsAny(base, "/\\\x00") || len(base) > 200 {
		return ""
	}
	return base
}

func fetchURL(url string, maxBytes int64) ([]byte, string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取下载内容失败: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("下载内容超过 %d 字节上限", maxBytes)
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return data, strings.TrimSpace(mime), nil
}
