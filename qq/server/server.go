// qq 服务的核心：把 deliver 动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包），并桥接 QQ 私聊消息 → user_message 任务链（NapCat/OneBot 11 正向 WS）。
// 零信任：仅主人私聊触发任务；群聊本批次未启用。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/qq/napcat"
	"github.com/NouSart/sump/service"
)

// Config 是 qq 服务的启动配置。
type Config struct {
	Name              string        // 服务名（注册用，默认 qq）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	NapCatURL         string        // NapCat 正向 WS 地址
	NapCatToken       string        // access_token（可选；作为秘密项预设由服务自己持有）
	Owner             string        // 主人 QQ（唯一授权私聊用户；留空则拒绝所有私聊；作为声明预设上报）
	Reasoner          string        // 任务跳转目标（reasoner 服务名，默认 reasoner）
	Images            string        // 图片服务名（默认 images）
	Files             string        // 文件服务名（默认 files）
	Memory            string        // 记忆服务名（默认 memory）：群消息"只记录不推理"时直接写进去
	GroupMode         string        // 群聊策略 observe / mention / judge（默认 judge）
	SecretFile        string        // 秘密项（napcat.token）的本地持久化路径（空 = data/<名>-secret.json；"-" = 不落盘）
}

// 群聊三档策略。默认 judge：群消息全部进上下文，说不说由模型按主人设置判断——
// 这正是"智能体知道自己在哪个群、谁在说话"的意义所在，入口不替它做决定。
const (
	GroupObserve = "observe" // 只记录不推理：群消息进记忆，不开推理轮次（省 token）
	GroupMention = "mention" // 没点到我就只记录
	GroupJudge   = "judge"   // 每条都进推理，回答与否由模型判断
)

// Server 是 qq 服务实例（服务骨架 + NapCat 客户端）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger
	napcat *napcat.Client

	setMu       sync.Mutex // 运行时可设项（设置下发 / 拉取即时生效）
	owner       string     // 当前主人 QQ（空 = 拒绝所有私聊）
	napcatURL   string     // 当前 NapCat WS 地址（热改后客户端立即重连）
	napcatToken string     // 当前 access_token（空 = 无；秘密项，值只在本服务手里）
	reasoner    string     // 当前任务跳转目标（reasoner 服务名）
	images      string     // 当前图片服务名
	files       string     // 当前文件服务名
	memory      string     // 当前记忆服务名（群消息只记录时直连它）
	groupMode   string     // 当前群聊策略

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex // 会话级串行锁（防同一会话消息乱序）

	groupsMu sync.Mutex
	groups   map[int64]groupMeta // 群资料缓存（群名 / 成员数 = 扩散面）

	titleMu sync.Mutex
	titled  map[string]bool // 本进程里已经试过起名的会话（失败不重复试）
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Reasoner == "" {
		cfg.Reasoner = "reasoner"
	}
	if cfg.Images == "" {
		cfg.Images = "images"
	}
	if cfg.Files == "" {
		cfg.Files = "files"
	}
	if cfg.Memory == "" {
		cfg.Memory = "memory"
	}
	if cfg.GroupMode == "" {
		cfg.GroupMode = GroupJudge
	}
	s := &Server{
		cfg: cfg, logger: logger,
		owner:       cfg.Owner,
		napcatURL:   cfg.NapCatURL,
		napcatToken: cfg.NapCatToken,
		reasoner:    cfg.Reasoner,
		images:      cfg.Images,
		files:       cfg.Files,
		memory:      cfg.Memory,
		groupMode:   cfg.GroupMode,
		locks:       map[string]*sync.Mutex{},
		groups:      map[int64]groupMeta{},
		titled:      map[string]bool{},
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "QQ",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		SecretFile:        cfg.SecretFile,
		Description:       "QQ 接入（NapCat/OneBot 11 · 私聊 + 群聊）：消息（带来源标注）→ 任务链；deliver → 发回 QQ",
		Provides: []protocol.Provide{
			{Action: "deliver", Input: "{text, conversation_id}", Output: "回执（已发回 QQ）"},
		},
		Logger: logger,
	})
	// 可设置项：预设 = 本服务的启动引导值（来源由服务自己决定）。
	s.HandleSetting("owner", cfg.Owner, func(v string) error {
		v = strings.TrimSpace(v)
		if v != "" {
			if _, err := strconv.ParseUint(v, 10, 64); err != nil {
				return fmt.Errorf("需要空串或纯数字 QQ 号，收到 %q", v)
			}
		}
		s.setMu.Lock()
		s.owner = v
		s.setMu.Unlock()
		return nil
	}, service.Label("只服务哪个 QQ 号"),
		service.Hint("填你自己的 QQ 号，只有它能指挥这个智能体；留空 = 谁的私聊都不理"),
		service.As("text"))
	s.HandleSetting("napcat.url", cfg.NapCatURL, func(v string) error {
		v = strings.TrimSpace(v)
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
			return fmt.Errorf("需要 ws:// 或 wss:// 地址，收到 %q", v)
		}
		s.setNapcatURL(v)
		return nil
	}, service.Label("NapCat 地址"),
		service.Hint("QQ 机器人网关的 WebSocket 地址，例如 ws://127.0.0.1:3000"),
		service.As("text"))
	s.HandleSetting("reasoner", cfg.Reasoner, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setReasoner(v)
		return nil
	}, service.Label("对话服务名"),
		service.Hint("QQ 来的消息交给哪个服务去回答"),
		service.As("service"))
	s.HandleSetting("images", cfg.Images, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setImages(v)
		return nil
	}, service.Label("图片服务名"),
		service.Hint("QQ 里发的图片存到哪个服务"),
		service.As("service"))
	s.HandleSetting("files", cfg.Files, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setFiles(v)
		return nil
	}, service.Label("文件服务名"),
		service.Hint("QQ 里发的文件存到哪个服务"),
		service.As("service"))
	s.HandleSetting("memory", cfg.Memory, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setMemory(v)
		return nil
	}, service.Label("记忆服务名"),
		service.Hint("群消息「只记录不推理」时直接写进哪个服务"),
		service.As("service"))
	s.HandleSetting("group.mode", cfg.GroupMode, func(v string) error {
		// 界面上传的是取值（observe / mention / judge）；人话在语言包里。
		switch strings.TrimSpace(v) {
		case GroupObserve, GroupMention, GroupJudge:
		default:
			return fmt.Errorf("群聊策略只能是 %s / %s / %s，收到 %q", GroupObserve, GroupMention, GroupJudge, v)
		}
		s.setGroupMode(v)
		return nil
	}, service.Label("群聊策略"),
		service.Hint("观察=群消息只进上下文不回答；仅@=被点到才回答，其余只记录；判断=每条都进上下文，说不说由智能体自己判断"),
		service.As("enum", GroupObserve, GroupMention, GroupJudge))
	s.Handle("deliver", s.actionDeliver)
	s.napcat = napcat.New(napcat.Config{
		URL:       cfg.NapCatURL,
		Token:     cfg.NapCatToken,
		Logger:    logger,
		OnMessage: s.onQQEvent,
	})
	// 秘密项：值不进中心，由本服务自己落盘 / 加载（Config.SecretFile）；启动参数给了也先应用一次
	//（与 reasoner 的 llm.key 同路径；秘密文件里若有值，Start 加载时会覆盖）。
	// 只有保存成功才切换端点并释放旧连接，失败的候选值不触发重连。
	s.HandleSecretWithCommit("napcat.token", nil, s.setNapcatToken,
		service.Label("NapCat 访问令牌"),
		service.Hint("网关开了鉴权才需要；值由本服务自己保存，中心不回显"))
	if cfg.NapCatToken != "" {
		s.setNapcatToken(cfg.NapCatToken)
	}
	// 只读项：启动参数带起来的，进面板可见但不可改（改它需要重启）。
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	return s
}

// ownerNow 返回当前主人 QQ（空 = 拒绝所有私聊）。
func (s *Server) ownerNow() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.owner
}

// setNapcatURL 更新 NapCat 地址并让客户端立即以新地址重连（token 原样保留）。
func (s *Server) setNapcatURL(v string) {
	s.setMu.Lock()
	s.napcatURL = v
	rawURL, token := s.napcatURL, s.napcatToken
	s.setMu.Unlock()
	s.napcat.SetEndpoint(rawURL, token)
}

// setNapcatToken 更新 access_token 并让客户端重连（token 只在握手时用得上，不重连不生效）。
func (s *Server) setNapcatToken(v string) {
	s.setMu.Lock()
	s.napcatToken = v
	rawURL, token := s.napcatURL, s.napcatToken
	s.setMu.Unlock()
	s.napcat.SetEndpoint(rawURL, token)
}

// reasonerName 返回当前任务跳转目标（后续 user_message 走新名）。
func (s *Server) reasonerName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.reasoner
}

// setReasoner 更新任务跳转目标。
func (s *Server) setReasoner(v string) {
	s.setMu.Lock()
	s.reasoner = v
	s.setMu.Unlock()
}

// imagesName 返回当前图片服务名（后续存图走新名）。
func (s *Server) imagesName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.images
}

// setImages 更新图片服务名。
func (s *Server) setImages(v string) {
	s.setMu.Lock()
	s.images = v
	s.setMu.Unlock()
}

// filesName 返回当前文件服务名（files 热改后 saveFile 立刻改投新服务）。
func (s *Server) filesName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.files
}

// setFiles 更新文件服务名。
func (s *Server) setFiles(v string) {
	s.setMu.Lock()
	s.files = v
	s.setMu.Unlock()
}

// memoryName 返回当前记忆服务名（群消息只记录时写进去的那个）。
func (s *Server) memoryName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.memory
}

// setMemory 更新记忆服务名。
func (s *Server) setMemory(v string) {
	s.setMu.Lock()
	s.memory = v
	s.setMu.Unlock()
}

// groupModeNow 返回当前群聊策略。
func (s *Server) groupModeNow() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.groupMode
}

// setGroupMode 更新群聊策略。
func (s *Server) setGroupMode(v string) {
	s.setMu.Lock()
	s.groupMode = v
	s.setMu.Unlock()
}

// Start 启动服务骨架，并额外启动 NapCat 连接循环（连不上会自动重试，不阻塞其余服务）。
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.Owner == "" {
		s.logger.Printf("警告: 未配置主人 QQ（-owner），所有私聊都将被拒绝；群聊仍按 group.mode=%s 处理", s.groupModeNow())
	}
	if err := s.Service.Start(ctx); err != nil {
		return err
	}
	go s.napcat.Run(ctx)
	return nil
}

// ---------- 动作 ----------

// actionDeliver 是 boss 约定动作：把任务结果发回原 QQ 会话（私聊或群聊）。
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
	target, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("deliver: 会话 id 无效 %q", id)
	}
	// 私聊与群聊只差一个参数名（user_id / group_id），send 里按 kind 选。
	if kind != "private" && kind != "group" {
		return nil, fmt.Errorf("deliver: 未知会话类型 %q（只认 private / group）", kind)
	}
	if err := s.send(ctx, kind, target, p.Text); err != nil {
		return nil, fmt.Errorf("发送 QQ 消息失败（%s）: %w", kind, err)
	}
	return map[string]any{}, nil
}

// ---------- QQ 消息 → 任务链 ----------

// onQQEvent 处理一条 NapCat 消息事件（napcat 客户端的回调 goroutine）。
//
// 两条路都由入口服务补上**来源标注**（谁是发送者、群还是私聊、扩散面多大），
// 这样记忆、模型上下文、门户看到的是同一句话；说不说、能不能外发由模型判断。
func (s *Server) onQQEvent(ev napcat.Event) {
	// 自己发出去的消息不回灌（否则"我回了 A，A 又被当成新消息"）。
	if ev.SelfID != 0 && ev.UserID == ev.SelfID {
		return
	}
	switch ev.MessageType {
	case "private":
		s.onPrivate(ev)
	case "group":
		s.onGroup(ev)
	default:
		s.logger.Printf("忽略未知消息类型: type=%q", ev.MessageType)
	}
}

// onPrivate 处理私聊：只认主人，其余礼貌拒绝且不触发任务。
func (s *Server) onPrivate(ev napcat.Event) {
	uid := strconv.FormatInt(ev.UserID, 10)
	owner := s.ownerNow()
	if owner == "" || uid != owner {
		s.logger.Printf("拒绝非主人私聊: user_id=%s", uid)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.send(ctx, "private", ev.UserID, "抱歉，你不是授权用户，已拒绝执行。"); err != nil {
			s.logger.Printf("发送拒绝语失败: %v", err)
		}
		return
	}

	// 会话级串行：同一会话逐条处理，不阻塞其他会话。
	lock := s.convLock("private:" + uid)
	lock.Lock()
	defer lock.Unlock()

	text, ids, fileIDs := s.collect(ev)
	if text == "" && len(ids) == 0 && len(fileIDs) == 0 {
		return
	}

	cid := "qq:private:" + uid
	sender := &protocol.Sender{
		Channel: "private", ID: uid, Name: ev.DisplayName(),
		Owner: true, Mentioned: true,
	}
	// 会话标题：私聊的名字只有一处来源（发送者），第一次见到就定下来。
	name := sender.Name
	if name == "" {
		name = uid
	}
	s.titleLater(cid, "QQ 私聊 · "+name)
	// 私聊里只有主人能进来，扩散面恒为"只有他"——不需要额外的策略附加信息。
	if err := s.callReasoner("user_message", protocol.UserMessagePayload{
		Text: text, ConversationID: cid, Images: ids, Files: fileIDs, Sender: sender,
	}); err != nil {
		s.logger.Printf("user_message 未受理: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err2 := s.send(ctx, "private", ev.UserID, "抱歉，服务暂时不可用。"); err2 != nil {
			s.logger.Printf("发送失败提示失败: %v", err2)
		}
	}
}

// onGroup 处理群聊：**任何人都能进来**（多人的上下文才叫群聊），
// 说不说由 group.mode 与模型判断，入口只负责把事实标清楚。
func (s *Server) onGroup(ev napcat.Event) {
	gid := strconv.FormatInt(ev.GroupID, 10)
	if gid == "0" {
		s.logger.Printf("群消息缺少 group_id（丢弃）: user_id=%d", ev.UserID)
		return
	}
	uid := strconv.FormatInt(ev.UserID, 10)

	lock := s.convLock("group:" + gid)
	lock.Lock()
	defer lock.Unlock()

	text, ids, fileIDs := s.collect(ev)
	if text == "" && len(ids) == 0 && len(fileIDs) == 0 {
		return
	}

	info := s.groupInfo(ev.GroupID)
	owner := s.ownerNow()
	cid := "qq:group:" + gid
	sender := &protocol.Sender{
		Channel: "group", ID: uid, Name: ev.DisplayName(),
		GroupID: gid, GroupName: info.Name, GroupSize: info.Members,
		Owner:     owner != "" && uid == owner,
		Mentioned: ev.Mentions(ev.SelfID),
	}

	// 群名是会话标题的一半（"QQ 群「编程交流」"）：取到名字时补上（每个会话只试一次）。
	// 取不到就不起名——按会话号兜底显示，比编一个"未知群"诚实。
	if info.Name != "" {
		s.titleLater(cid, "QQ 群「"+info.Name+"」")
	}

	if !s.shouldReason(sender) {
		s.logger.Printf("群 %s：消息按策略（%s）只记录不推理（from=%s 提及=%v）", gid, s.groupModeNow(), uid, sender.Mentioned)
		s.storeQuietly(cid, sender, text, ids, fileIDs)
		return
	}
	if err := s.callReasoner("user_message", protocol.UserMessagePayload{
		Text: text, ConversationID: cid, Images: ids, Files: fileIDs, Sender: sender,
	}); err != nil {
		s.logger.Printf("群 %s：user_message 未受理: %v", gid, err)
		if sender.Mentioned {
			// 被点名了却没接住：在群里说一声，别让人干等。
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err2 := s.send(ctx, "group", ev.GroupID, "抱歉，服务暂时不可用。"); err2 != nil {
				s.logger.Printf("群 %s：发送失败提示失败: %v", gid, err2)
			}
		}
	}
}

// shouldReason 判断这条群消息要不要开推理轮次（策略在入口侧，模型看不见被拦下的消息）。
func (s *Server) shouldReason(sender *protocol.Sender) bool {
	switch s.groupModeNow() {
	case GroupObserve:
		return false
	case GroupMention:
		return sender.Mentioned
	default: // GroupJudge
		return true
	}
}

// collect 把一条消息里的文字、图片、文件整理成任务链需要的样子。
func (s *Server) collect(ev napcat.Event) (string, []string, []string) {
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
	return text, ids, fileIDs
}

// saveImage 把图片 URL 交给 images 服务保存，返回图片 id。
func (s *Server) saveImage(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := s.Call(ctx, s.imagesName(), "save", protocol.ImageSavePayload{URL: url}, protocol.NewID(), s.cfg.Name)
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
	data, err := s.Call(ctx, s.filesName(), "save", protocol.FileSavePayload{URL: fr.URL, Filename: name}, protocol.NewID(), s.cfg.Name)
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

// callReasoner 向 reasoner 发 user_message（等受理回执；boss=本服务）。
func (s *Server) callReasoner(action string, payload any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := s.Call(ctx, s.reasonerName(), action, payload, protocol.NewID(), s.cfg.Name)
	return err
}

// titleLater 给会话起一个人话标题（门户侧栏显示的就是它）：每个会话只试一次，
// 并且**不挡消息本身的路**——起名是锦上添花，记忆服务抖动时也不能让聊天卡住。
func (s *Server) titleLater(cid, title string) {
	if title == "" {
		return
	}
	s.titleMu.Lock()
	done := s.titled[cid]
	s.titled[cid] = true
	s.titleMu.Unlock()
	if done {
		return
	}
	go s.titleConversation(cid, title)
}

// titleConversation 调 memory 的 conversation_open 把标题交上去（只在标题为空时生效）。
func (s *Server) titleConversation(cid, title string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := s.Call(ctx, s.memoryName(), "conversation_open", protocol.ConversationOpenPayload{
		ID:    cid,
		Title: title,
	}, protocol.NewID(), s.cfg.Name)
	if err != nil {
		s.logger.Printf("会话 %s：起名（%s）失败: %v", cid, title, err)
	}
}

// send 发一条 QQ 消息：kind = private / group。
// 参数名照 NapCat（OneBot 11）：私聊用 user_id，群聊用 group_id。
func (s *Server) send(ctx context.Context, kind string, target int64, text string) error {
	params := map[string]any{"message_type": kind, "message": text}
	if kind == "group" {
		params["group_id"] = target
	} else {
		params["user_id"] = target
	}
	_, err := s.napcat.Call(ctx, "send_msg", params)
	return err
}

// ---------- 群资料（扩散面）----------

// groupMeta 是一个群的资料缓存。
type groupMeta struct {
	Name    string
	Members int
	at      time.Time
}

// groupInfoTTL 是群资料缓存的有效期：群名会改、人数会变，缓存只是为了别每条消息都去问一次。
const groupInfoTTL = 10 * time.Minute

// groupInfoRetry 是"没取到"的缓存期：网关不支持或一时不通时，别让每条消息都等一次超时。
const groupInfoRetry = time.Minute

// groupInfo 取群资料（带缓存）：群名与成员数是来源标注里"扩散面"的一半。
// 取不到就返回空资料——宁可写"人数未知"，也不编一个数字（过一会儿再试）。
func (s *Server) groupInfo(groupID int64) groupMeta {
	s.groupsMu.Lock()
	meta, ok := s.groups[groupID]
	s.groupsMu.Unlock()
	if ok {
		ttl := groupInfoTTL
		if meta.Name == "" {
			ttl = groupInfoRetry // 上次没取到：短缓存，别把"未知"锁十分钟
		}
		if meta.at.Add(ttl).After(time.Now()) {
			return meta
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := s.napcat.GroupInfo(ctx, groupID)
	if err != nil {
		s.logger.Printf("群 %d 资料取不到（按未知处理）: %v", groupID, err)
	}
	meta = groupMeta{Name: info.Name, Members: info.Members, at: time.Now()}
	s.groupsMu.Lock()
	s.groups[groupID] = meta
	s.groupsMu.Unlock()
	return meta
}

// storeQuietly 把一条"只记录不推理"的消息写进记忆：上下文里留痕，但不烧推理轮次。
//
// 正文带上与推理侧同样的来源标注（Sender.Label 是协议里的唯一实现），
// 这样以后模型召回这段历史时知道这句话是谁在群里说的；
// 任务名显式给出（否则会从标注那一行推出一个像标签的任务名）。
func (s *Server) storeQuietly(cid string, sender *protocol.Sender, text string, ids, fileIDs []string) {
	label := sender.Label()
	content := text
	if label != "" {
		if content == "" {
			content = label
		} else {
			content = label + " " + content
		}
	}
	meta, _ := json.Marshal(map[string]any{
		"sender": sender, "images": ids, "files": fileIDs,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.Call(ctx, s.memoryName(), "store", protocol.StorePayload{
		ConversationID: cid,
		Boss:           s.cfg.Name,
		Role:           "user",
		Kind:           "user",
		Content:        content,
		Meta:           string(meta),
		TaskName:       taskNameOf(text, ids, fileIDs),
	}, protocol.NewID(), s.cfg.Name)
	if err != nil {
		s.logger.Printf("会话 %s：只记录模式落库失败: %v", cid, err)
	}
}

// taskNameOf 从消息正文里取一个任务名；正文为空（只发了图 / 文件）时按附件类型兜底——
// 否则任务名会退化成来源标注那一行。名字的截断规则只有一份（protocol.ClampTaskName），
// 这样 qq 直接落库与 reasoner 代写记忆得到的是同一个名字。
func taskNameOf(text string, images, files []string) string {
	if name := protocol.ClampTaskName(strings.SplitN(text, "\n", 2)[0]); name != "" {
		return name
	}
	switch {
	case len(images) > 0:
		return "[图片]"
	case len(files) > 0:
		return "[文件]"
	}
	return ""
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
