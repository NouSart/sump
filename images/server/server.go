// images 服务的核心：把图片动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包）。
// save：从 URL 或 base64 保存图片；fetch：把图片转成 base64 发回调用方（llm 取图用）。
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NouSart/sump/images/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// Config 是 images 服务的启动配置。
type Config struct {
	Name              string        // 服务名（注册用，默认 images）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	Store             *store.Store  // 图片库
	MaxBytes          int64         // 单图上限（默认 32MiB，对齐 DeepSeek 图片限制；作为声明预设上报）
	DBPath            string        // 元数据库路径（只读项展示用：换它要重启）
	Dir               string        // 图片二进制目录（只读项展示用：换它要重启）
}

// 默认单图上限（32MiB）。
const defaultMaxBytes = 32 << 20

// Server 是 images 服务实例（服务骨架 + 图片库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu    sync.Mutex // 运行时可设项（设置下发 / 拉取即时生效）
	maxBytes int64      // 当前单图上限（字节）
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxBytes < 1<<20 {
		cfg.MaxBytes = 1 << 20 // 声明以 MB 计——小于 1MiB 的引导值归一到 1MiB
	}
	s := &Server{cfg: cfg, logger: logger, maxBytes: cfg.MaxBytes}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "图片",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		ActionTimeout:     60 * time.Second, // save 含下载，放宽动作超时
		Description:       "图片服务：保存图片（URL/base64），按需转 base64 发回调用方",
		Provides: []protocol.Provide{
			{Action: "save", Tool: true, Input: "{url} 或 {data(base64), mime}", Output: "{id, mime, size}"},
			{Action: "fetch", Tool: true, Input: "{id, x1?,y1?,x2?,y2?, max_side?}（不带坐标 = 缩略图，长边 ≤ max_side 默认 768；给矩形 = 该区域的原图，坐标是原图像素）", Output: "{id, mime, data(base64), size, original_width, original_height, width, height, x1,y1,x2,y2, downscaled, data_bytes}（原图尺寸与本次窗口都回传，方便平移）"},
			{Action: "fetch_full", Input: "{id}", Output: "{id, mime, data(base64), size}（原图，不做任何处理；服务间调用专用，不给模型）"},
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
	}, service.Label("单张图片大小上限"),
		service.Hint("超过这个大小的图片会被拒绝"),
		service.As("number"),
		service.Unit("MB"))
	s.Handle("save", s.actionSave)
	s.Handle("fetch", s.actionFetch)
	s.Handle("fetch_full", s.actionFetchFull)
	// 只读项：启动参数带起来的（换它要重启服务）。db/dir 是存储身份——运行中换库/换目录
	// 意味着一批在途读写换个落点，不做（宁可不给旋钮，也不给一个会撕开的旋钮）。
	s.HandleReadOnly("db", cfg.DBPath)
	s.HandleReadOnly("dir", cfg.Dir)
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	return s
}

// maxBytesNow 返回当前单图上限（字节）。
func (s *Server) maxBytesNow() int64 {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.maxBytes
}

// ---------- 动作 ----------

// actionSave 保存一张图片：URL 或 base64 二选一。
func (s *Server) actionSave(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ImageSavePayload
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
		return nil, errors.New("save: 图片为空")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("save: 图片超过 %d 字节上限", limit)
	}
	mime := p.Mime
	if mime == "" {
		mime = mimeHint
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	id, err := s.cfg.Store.Save(data, mime)
	if err != nil {
		return nil, fmt.Errorf("保存失败: %w", err)
	}
	s.logger.Printf("已保存图片 %s（%s, %d 字节）", id, mime, len(data))
	return protocol.ImageSaveResult{ID: id, Mime: mime, Size: int64(len(data))}, nil
}

// actionFetch 取一张图，把图片转成 base64 发回调用方。**永远不给原图**——这是模型走的那条路。
//
// 两种取法（见 protocol.ImageFetchPayload）：
//   - 给矩形（x1,y1,x2,y2）：该区域的原图，超上限等比缩小
//   - 什么参数都不给：整图缩略图（长边 ≤ max_side，默认 768）
//
// 为什么要缩略图/窗口：整张 8MB 截图 base64 之后 ~11MB，一次就把模型上下文撑爆。
// 缩略图先让模型看清「大概是什么」，要看细节再用矩形平移窗口取局部原图。
// 要原图的是服务（reasoner 内联用户刚发的图），走 fetch_full——那个动作不进工具列表。
func (s *Server) actionFetch(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ImageFetchPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("fetch: id 必填")
	}
	data, mime, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}

	img, format, err := decodeImage(data)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w（要原图请用 fetch_full）", err)
	}
	b := img.Bounds()
	maxSide := p.MaxSide
	if maxSide <= 0 {
		maxSide = defaultMaxSide
	}
	if maxSide < minMaxSide {
		maxSide = minMaxSide
	}
	if maxSide > maxMaxSide {
		maxSide = maxMaxSide
	}

	rect, cropped, err := cropRect(img, p.X1, p.Y1, p.X2, p.Y2)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	out := img
	if cropped && rect != b {
		out = cropImage(img, rect)
	}
	before := out.Bounds()
	out, scaled := scaleToFit(out, maxSide)
	enc, w, h, shrunk, err := encodeFit(out, format == "jpeg")
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	if !scaled && !cropped && len(enc) >= len(data) {
		// 原图本来就小（索引色 PNG 之类）：重编码成 JPEG 只会更大，原样发回。
		return protocol.ImageFetchResult{
			ID: p.ID, Mime: mime, Data: base64.StdEncoding.EncodeToString(data), Size: int64(len(data)),
			OriginalWidth: b.Dx(), OriginalHeight: b.Dy(), Width: b.Dx(), Height: b.Dy(),
			X1: rect.Min.X, Y1: rect.Min.Y, X2: rect.Max.X, Y2: rect.Max.Y, DataBytes: len(data),
		}, nil
	}
	return protocol.ImageFetchResult{
		ID: p.ID, Mime: "image/jpeg", Data: base64.StdEncoding.EncodeToString(enc), Size: int64(len(data)),
		OriginalWidth: b.Dx(), OriginalHeight: b.Dy(),
		Width: w, Height: h,
		X1: rect.Min.X, Y1: rect.Min.Y, X2: rect.Max.X, Y2: rect.Max.Y,
		Downscaled: scaled || shrunk || before.Dx() != w || before.Dy() != h,
		DataBytes:  len(enc),
	}, nil
}

// actionFetchFull 原样发回整张图（服务间调用：reasoner 内联用户刚发的图）。
// 不进工具列表（provides 里不带 tool:true）：模型拿不到这条逃逸通道，才不会一把撑爆上下文。
func (s *Server) actionFetchFull(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ImageFetchPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("fetch_full: id 必填")
	}
	data, mime, err := s.cfg.Store.Load(p.ID)
	if err != nil {
		return nil, err
	}
	return protocol.ImageFetchResult{
		ID: p.ID, Mime: mime, Data: base64.StdEncoding.EncodeToString(data), Size: int64(len(data)),
		DataBytes: len(data),
	}, nil
}

// fetchURL 下载图片：限时 30s、限制大小（多读 1 字节探测超限）。
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
		maxBytes = 32 << 20
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
