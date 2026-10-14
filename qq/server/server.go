// qq 服务的核心：把 deliver 动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包），并桥接 QQ 私聊消息 → user_message 任务链（NapCat/OneBot 11 正向 WS）。
// 零信任：仅主人私聊触发任务；群聊本批次未启用。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/qq/napcat"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// Config 是 qq 服务的启动配置。
type Config struct {
	Name              string        // 服务名（注册用，默认 qq）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	NapCatURL         string        // NapCat 正向 WS 地址
	NapCatToken       string        // access_token（可选）
	Owner             string        // 主人 QQ（唯一授权私聊用户；留空则拒绝所有私聊）
	Agent             string        // 任务跳转目标（reasoner 服务名，默认 reasoner）
	Images            string        // 图片服务名（默认 images）
	Files             string        // 文件服务名（默认 files）
}

// Server 是 qq 服务实例（服务骨架 + NapCat 客户端）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger
	napcat *napcat.Client

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // 会话级串行锁（防同一会话消息乱序）
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Agent == "" {
		cfg.Agent = "reasoner"
	}
	if cfg.Images == "" {
		cfg.Images = "images"
	}
	if cfg.Files == "" {
		cfg.Files = "files"
	}
	s := &Server{cfg: cfg, logger: logger, locks: map[string]*sync.Mutex{}}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "QQ 接入（NapCat/OneBot 11 · 仅私聊）：消息 → 任务链；deliver → 发回 QQ",
		Provides: []protocol.Provide{
			{Action: "deliver", Input: "{text, conversation_id}", Output: "回执（已发回 QQ）"},
		},
		Settings: []protocol.Setting{{Key: "conn.default_ttl", Default: "5m"}},
		Logger:   logger,
	})
	s.Handle("deliver", s.actionDeliver)
	s.napcat = napcat.New(napcat.Config{
		URL:       cfg.NapCatURL,
		Token:     cfg.NapCatToken,
		Logger:    logger,
		OnMessage: s.onQQEvent,
	})
	return s
}

// Start 启动服务骨架，并额外启动 NapCat 连接循环（连不上会自动重试，不阻塞其余服务）。
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.Owner == "" {
		s.logger.Printf("警告: 未配置主人 QQ（-owner），所有私聊都将被拒绝")
	}
	if err := s.Service.Start(ctx); err != nil {
		return err
	}
	go s.napcat.Run(ctx)
	return nil
}

// ---------- 动作 ----------

// actionDeliver 是 boss 约定动作：把任务结果发回原 QQ 会话。
func (s *Server) actionDeliver(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DeliverPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" {
		return nil, errors.New("deliver: text 为空")
	}
	kind, id, ok := parseConversation(p.ConversationID)
	if !ok {
		return nil, fmt.Errorf("deliver: 无法解析会话标识 %q", p.ConversationID)
	}
	if kind != "private" {
		return nil, errors.New("deliver: 群聊未启用")
	}
	uid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("deliver: 会话 id 无效 %q", id)
	}
	if err := s.sendPrivate(ctx, uid, p.Text); err != nil {
		return nil, fmt.Errorf("发送 QQ 消息失败: %w", err)
	}
	return map[string]any{}, nil
}

// ---------- QQ 私聊 → 任务链 ----------

// onQQEvent 处理一条 NapCat 消息事件（napcat 客户端的回调 goroutine）。
func (s *Server) onQQEvent(ev napcat.Event) {
	if ev.MessageType != "private" {
		s.logger.Printf("忽略非私聊消息（群聊未启用）: type=%s group=%d", ev.MessageType, ev.GroupID)
		return
	}
	uid := strconv.FormatInt(ev.UserID, 10)
	if s.cfg.Owner == "" || uid != s.cfg.Owner {
		s.logger.Printf("拒绝非主人私聊: user_id=%s", uid)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.sendPrivate(ctx, ev.UserID, "抱歉，你不是授权用户，已拒绝执行。"); err != nil {
			s.logger.Printf("发送拒绝语失败: %v", err)
		}
		return
	}

	// 会话级串行：同一会话逐条处理，不阻塞其他会话。
	lock := s.convLock(uid)
	lock.Lock()
	defer lock.Unlock()

	text := ev.Text()
	var ids []string
	for _, u := range ev.ImageURLs() {
		id, err := s.saveImage(u)
		if err != nil {
			s.logger.Printf("保存图片失败: %v", err)
			if strings.TrimSpace(text) == "" {
				text = "[图片]"
			} else {
				text += "\n[图片]"
			}
			continue
		}
		ids = append(ids, id)
	}
	var fileIDs []string
	for _, fr := range ev.FileRefs() {
		id, err := s.saveFile(fr)
		if err != nil {
			s.logger.Printf("保存文件失败: %v", err)
			text = appendLine(text, "[文件]")
			continue
		}
		fileIDs = append(fileIDs, id)
		// 占位必须带 id——文件是模型凭 id 调 files__read 自取的（与图片的自动内联不同）。
		text = appendLine(text, fmt.Sprintf("[文件: %s, id=%s]", fr.Name, id))
	}
	if strings.TrimSpace(text) == "" && len(ids) == 0 && len(fileIDs) == 0 {
		return
	}

	cid := "qq:private:" + uid
	payload := protocol.UserMessagePayload{Text: text, ConversationID: cid, Images: ids, Files: fileIDs}
	if err := s.callAgent("user_message", payload); err != nil {
		s.logger.Printf("user_message 未受理: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err2 := s.sendPrivate(ctx, ev.UserID, "抱歉，服务暂时不可用。"); err2 != nil {
			s.logger.Printf("发送失败提示失败: %v", err2)
		}
	}
}

// saveImage 把图片 URL 交给 images 服务保存，返回图片 id。
func (s *Server) saveImage(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := s.Call(ctx, s.cfg.Images, "save", protocol.ImageSavePayload{URL: url}, protocol.NewID(), s.cfg.Name)
	if err != nil {
		return "", err
	}
	var res protocol.ImageSaveResult
	if err := protocol.DecodeRaw(data, &res); err != nil || res.ID == "" {
		return "", errors.New("images.save 返回无效")
	}
	return res.ID, nil
}

// saveFile 把文件 URL 交给 files 服务保存，返回文件 id。
func (s *Server) saveFile(fr napcat.FileRef) (string, error) {
	name := strings.TrimSpace(fr.Name)
	if name == "" {
		name = "未命名文件"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	data, err := s.Call(ctx, s.cfg.Files, "save", protocol.FileSavePayload{URL: fr.URL, Filename: name}, protocol.NewID(), s.cfg.Name)
	if err != nil {
		return "", err
	}
	var res protocol.FileSaveResult
	if err := protocol.DecodeRaw(data, &res); err != nil || res.ID == "" {
		return "", errors.New("files.save 返回无效")
	}
	return res.ID, nil
}

// appendLine 向文本追加一行（空文本直接起行）。
func appendLine(text, line string) string {
	if strings.TrimSpace(text) == "" {
		return line
	}
	return text + "\n" + line
}

// callAgent 向 reasoner 发 user_message（等受理回执；boss=本服务）。
func (s *Server) callAgent(action string, payload any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := s.Call(ctx, s.cfg.Agent, action, payload, protocol.NewID(), s.cfg.Name)
	return err
}

// sendPrivate 私聊发送一条文本。
func (s *Server) sendPrivate(ctx context.Context, uid int64, text string) error {
	_, err := s.napcat.Call(ctx, "send_msg", map[string]any{
		"message_type": "private",
		"user_id":      uid,
		"message":      text,
	})
	return err
}

// parseConversation 解析会话标识（qq:private:<uid> / qq:group:<gid>）。
func parseConversation(cid string) (kind, id string, ok bool) {
	parts := strings.SplitN(cid, ":", 3)
	if len(parts) != 3 || parts[0] != "qq" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// convLock 返回会话级互斥锁。
func (s *Server) convLock(key string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	l := s.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}
