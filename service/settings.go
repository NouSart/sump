// 设置能力（骨架统一实现）：服务声明可设置项（键 + 预设值 + 校验/应用回调），
// 骨架负责：注册名片带上声明、承接中心下发的 configure（注册后全量逐键、变更单键）、
// 注册成功后主动拉取一次作为兜底（与下发幂等）。
// 校验权在服务侧：回调返回错误即拒绝，服务保持旧值。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// settingKind 是声明类别：普通项（值由设置中心保管）／秘密项（值由本服务保管）／只读项（不可改）。
type settingKind int

const (
	settingNormal settingKind = iota
	settingSecret
	settingReadOnly
)

// settingEntry 是一个已声明的设置项。
type settingEntry struct {
	key    string
	preset string
	kind   settingKind
	apply  func(value string) error
	// 给**人**看的部分（可选）：中文名、一句话说明、控件类型、枚举候选。
	label   string
	hint    string
	input   string
	options []string
}

// Option 给设置项补"人话"元数据。不传就用 Key 当标题、纯文本框。
type Option func(*settingEntry)

// Label 设置中文名（如「单文件大小上限」）。
func Label(text string) Option { return func(e *settingEntry) { e.label = text } }

// Hint 设置一句话说明：改它会发生什么、取值范围、留空意味着什么。
func Hint(text string) Option { return func(e *settingEntry) { e.hint = text } }

// As 指定控件类型（bool / number / enum / textarea / text），枚举顺带给候选值。
func As(kind string, options ...string) Option {
	return func(e *settingEntry) {
		e.input = kind
		e.options = options
	}
}

// readOnlyLabels 是启动参数的中文名：这些键在所有服务里同义，写一次大家共用。
var readOnlyLabels = map[string]string{
	"name":     "服务名",
	"addr":     "监听地址",
	"center":   "设置中心",
	"db":       "数据文件",
	"dir":      "数据目录",
	"web":      "前端目录",
	"settings": "设置清单",
	"reasoner": "对话服务",
	"memory":   "记忆服务",
	"images":   "图片服务",
	"files":    "文件服务",
	"portal":   "门户服务",
	"approval": "审批服务",
}

// HandleSetting 声明一个可设置项：预设值 + 校验/应用回调（必须在 Start 前调用）。
// 预设值由服务自己决定（库不规定来源）；回调返回错误 = 明确拒绝该值，服务保持旧值。
// 值由**设置中心**保管（覆盖值落盘于中心），注册时全量对齐下发。
func (s *Service) HandleSetting(key, preset string, apply func(value string) error, opts ...Option) {
	s.declare(key, preset, settingNormal, apply, opts...)
}

// HandleSecret 声明一个**秘密项**：值不进设置中心——中心不落盘、不回显、list 恒回空串，
// 只在面板提交那一次把值转发过来；由**本服务自己**持久化（Config.SecretFile）并在 Start
// 时加载（中心那边没有值可下发）。一句话：谁消费谁持有。
func (s *Service) HandleSecret(key string, apply func(value string) error, opts ...Option) {
	s.declare(key, "", settingSecret, apply, opts...)
}

// HandleReadOnly 声明一个**只读项**：启动参数带起来的值，进面板可见但不可改
// （改动会被明确拒绝——改它要重启，或等服务重启机制）。
// 中文名按 readOnlyLabels 自动补（name / addr / center / db …），特殊键用 Label 覆盖。
func (s *Service) HandleReadOnly(key, value string, opts ...Option) {
	s.declare(key, value, settingReadOnly, nil, opts...)
}

// declare 是三种声明的公共落点（时机校验、去重、登记生效值）。
func (s *Service) declare(key, preset string, kind settingKind, apply func(value string) error, opts ...Option) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		s.logger.Printf("声明设置项(%s) 必须在 Start 前调用——已忽略", key)
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if _, dup := s.settingIndex[key]; dup {
		s.logger.Printf("设置项重复声明（忽略）: %s", key)
		return
	}
	s.settingIndex[key] = len(s.settings)
	entry := settingEntry{key: key, preset: preset, kind: kind, apply: apply}
	for _, opt := range opts {
		opt(&entry)
	}
	// 只读启动参数：中文名按公共表自动补（name→服务名、addr→监听地址 …），说明统一。
	if kind == settingReadOnly {
		if entry.label == "" {
			if label, ok := readOnlyLabels[key]; ok {
				entry.label = label
			}
		}
		if entry.hint == "" {
			entry.hint = "启动参数，改它要重启这个服务"
		}
	}
	s.settings = append(s.settings, entry)
	s.settingValues[key] = preset
}

// SettingValue 返回某设置项最近一次成功应用的生效值（未下发/拉取过时 = 声明预设）。
// 秘密项返回的是**本服务自己**持有的值（中心那边没有）；日志/名片仍不回显。
func (s *Service) SettingValue(key string) string {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.settingValues[key]
}

// isSecret 判断某键是否声明为秘密项（日志据此决定要不要回显值）。
func (s *Service) isSecret(key string) bool {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	idx, ok := s.settingIndex[key]
	return ok && s.settings[idx].kind == settingSecret
}

// AnnounceSetting 把某项的当前生效值回传给设置中心——服务侧的值**自己变了**时调用它
// （自适应 / 规范化 / 程序内部改写），中心据此更新「服务侧实际值」，面板实时可见，
// 不必等服务下次注册。只报事实：异步发出（不阻塞本服务）、失败不排队
// （下次注册的名片对齐会收敛）；秘密项只回「有没有值」，值本身永不上线。
func (s *Service) AnnounceSetting(key string) {
	s.settingsMu.Lock()
	idx, ok := s.settingIndex[key]
	p := protocol.ReportSettingPayload{Service: s.cfg.Name, Key: key}
	if ok {
		if s.settings[idx].kind == settingSecret {
			p.Set = s.secrets[key] != ""
		} else {
			p.Value = s.settingValues[key]
		}
		s.announceSeq++
		p.Seq = s.announceSeq
	}
	s.settingsMu.Unlock()

	if !ok {
		s.logger.Printf("AnnounceSetting 忽略：未声明的设置项 %s", key)
		return
	}
	s.Fire("settings-center", "report_setting", p, "", "")
}

// declarations 返回名片用的设置声明（按声明顺序）。
// 秘密项**只报有没有值**（Set），值本身永不上线；只读项把启动值放在 default 里。
func (s *Service) declarations() []protocol.Setting {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	out := make([]protocol.Setting, 0, len(s.settings))
	for _, it := range s.settings {
		view := protocol.Setting{
			Key: it.key, Label: it.label, Hint: it.hint, Kind: it.input, Options: it.options,
		}
		switch it.kind {
		case settingSecret:
			view.Secret = true
			view.Set = s.secrets[it.key] != ""
		case settingReadOnly:
			view.Default = it.preset
			view.ReadOnly = true
		default:
			view.Default = it.preset
		}
		out = append(out, view)
	}
	return out
}

// actionConfigure 承接设置中心下发的设置变更（configure）：立即校验并应用（不等重启）。
func (s *Service) actionConfigure(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ConfigurePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if err := s.applySetting(p.Key, p.Value); err != nil {
		return nil, err
	}
	// 秘密项的值不进日志（日志会被人看、会被转发，值只该留在服务自己的文件里）
	if s.isSecret(p.Key) {
		s.logger.Printf("设置生效（下发）: %s（秘密，不回显）", p.Key)
	} else {
		s.logger.Printf("设置生效（下发）: %s=%s", p.Key, p.Value)
	}
	return map[string]any{"status": "applied"}, nil
}

// applySetting 校验并应用一个设置项；键未声明或回调拒绝即返回错误（保持旧值）。
// 应用串行化：同一服务的多个下发不并发进回调。
func (s *Service) applySetting(key, value string) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.settingsMu.Lock()
	idx, ok := s.settingIndex[key]
	var apply func(string) error
	kind := settingNormal
	if ok {
		apply = s.settings[idx].apply
		kind = s.settings[idx].kind
	}
	s.settingsMu.Unlock()
	if !ok {
		return fmt.Errorf("configure: 未声明的设置项 %q", key)
	}
	if kind == settingReadOnly {
		return fmt.Errorf("configure: %s 是只读项（启动参数带起来的，改它需要重启）", key)
	}

	if apply != nil {
		if err := apply(value); err != nil {
			return fmt.Errorf("configure: %s: %w", key, err)
		}
	}
	// 秘密项不进中心：由本服务自己落盘（普通项的覆盖值由中心保管）。
	if kind == settingSecret {
		if err := s.saveSecret(key, value); err != nil {
			return fmt.Errorf("configure: %s: 秘密落盘失败: %w", key, err)
		}
	}
	s.settingsMu.Lock()
	s.settingValues[key] = value
	s.settingsMu.Unlock()
	return nil
}

// RefreshSettings 主动拉取一次本服务的设置（只应用覆盖项；手动兜底用）。
func (s *Service) RefreshSettings(ctx context.Context) { s.refreshSettings(ctx) }

// refreshSettings 拉取并应用本服务的覆盖项（注册成功后骨架自动调用；与下发幂等）。
func (s *Service) refreshSettings(ctx context.Context) {
	if !s.hasSettings() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := s.Call(cctx, "settings-center", "list_settings", protocol.SettingsListPayload{Service: s.cfg.Name}, s.cfg.Name, "")
	if err != nil {
		s.logger.Printf("拉取设置失败（沿用当前值）: %v", err)
		return
	}
	var res protocol.SettingsResult
	if err := protocol.DecodeRaw(data, &res); err != nil {
		s.logger.Printf("设置解析失败（沿用当前值）: %v", err)
		return
	}
	for _, svc := range res.Services {
		if svc.Service != s.cfg.Name {
			continue
		}
		for _, item := range svc.Settings {
			if !item.Overridden {
				continue
			}
			if err := s.applySetting(item.Key, item.Value); err != nil {
				s.logger.Printf("设置拉取应用失败: %v", err)
				continue
			}
			s.logger.Printf("设置生效（拉取）: %s=%s", item.Key, item.Value)
		}
	}
}

func (s *Service) hasSettings() bool {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return len(s.settings) > 0
}
