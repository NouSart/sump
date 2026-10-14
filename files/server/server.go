// files 服务的核心：把文件动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包）。
// save：从 URL 或 base64 保存文件；fetch：把文件转成 base64 发回调用方；
// read：读取文本类文件的内容（超长按 read.max_chars 截断）——声明 tool:true，
// 经名册自动成为 LLM 工具（files__read），模型凭占位文本里的 id 自取。
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/files/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
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
}

// Server 是 files 服务实例（服务骨架 + 文件库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu        sync.Mutex // 运行时可设项（设置下发 / 拉取即时生效）
	maxBytes     int64      // 当前单文件上限（字节）
	readMaxChars int        // 当前 read 截断阈值（字符数）
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
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ActionTimeout:     60 * time.Second, // save 含下载，放宽动作超时
		Description:       "文件服务：保存文件（URL/base64），按需取回（fetch）或读取文本内容（read）",
		Provides: []protocol.Provide{
			{Action: "save", Input: "{url} 或 {data(base64), filename, mime}", Output: "{id, filename, mime, size}"},
			{Action: "fetch", Input: "{id}", Output: "{id, filename, mime, data(base64), size}"},
			{Action: "read", Input: "{id}", Output: "{id, filename, text, truncated}（文本类；二进制报错）", Tool: true},
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
	})
	s.HandleSetting("read_max_chars", strconv.Itoa(cfg.ReadMaxChars), func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return fmt.Errorf("需要 ≥1 的整数（字符数），收到 %q", v)
		}
		s.setMu.Lock()
		s.readMaxChars = int(n)
		s.setMu.Unlock()
		return nil
	})
	s.Handle("save", s.actionSave)
	s.Handle("fetch", s.actionFetch)
	s.Handle("read", s.actionRead)
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
	if filename == "" {
		return nil, errors.New("save: filename 必填")
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

// actionRead 读取文本类文件的内容（LLM 工具）：先 Stat 判定类型再读本体，
// 超长按字符数截断并置 truncated——“没给全”必须显式告知模型。
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
	if !isTextual(meta.Mime, meta.Filename) {
		return nil, fmt.Errorf("read: 该文件类型（%s）暂不支持阅读，仅支持文本类", meta.Mime)
	}
	data, _, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("read: %s 的内容不是有效的 UTF-8 文本", meta.Filename)
	}
	text, truncated := truncateRunes(string(data), s.readMaxCharsNow())
	s.logger.Printf("已读取文件 %s（%s, %d 字节, 截断=%v）", p.ID, meta.Filename, len(data), truncated)
	return protocol.FileReadResult{ID: p.ID, Filename: meta.Filename, Text: text, Truncated: truncated}, nil
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
