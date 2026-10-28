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
	key     string
	preset  string
	kind    settingKind
	apply   func(value string) error
	version uint64 // 进程内版本；成功改变生效值时递增，不进入设置协议或日志。
	// commit 非空时，apply 只校验；秘密保存成功后才发布运行时状态。
	commit func(value string)
	// 给**人**看的部分（可选）：中文名、一句话说明、控件类型、枚举候选、数字单位。
	label   string
	hint    string
	input   string
	options []string
	unit    string
}

// Option 给设置项补"人话"元数据。不传就用 Key 当标题、纯文本框。
type Option func(*settingEntry)

// Label 设置中文名（如「单文件大小上限」）。
func Label(text string) Option { return func(e *settingEntry) { e.label = text } }

// Hint 设置一句话说明：改它会发生什么、取值范围、留空意味着什么。
func Hint(text string) Option { return func(e *settingEntry) { e.hint = text } }

// As 指定控件类型（bool / number / enum / textarea / text / service / optional-service），
// 枚举顺带给候选值。
//
// service 表示值是一个**服务名**：面板从名册里列候选，但最终校验权仍在本服务
// （名册只是候选，不是契约）。optional-service 表示该项可以"没有"（清空即关闭这项能力），
// 面板会画成「开关 + 选择」。
func As(kind string, options ...string) Option {
	return func(e *settingEntry) {
		e.input = kind
		e.options = options
	}
}

// Unit 给数字项标单位符号：ms / MB / KB / chars / count。
//
// 声明的是**符号**而不是中文——面板负责翻成人话（"毫秒"），并在值旁边加一句读法
// （300000 ms → ≈ 5 分钟）。服务仍然只认原始数值。
func Unit(symbol string) Option { return func(e *settingEntry) { e.unit = symbol } }

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
// 回调若会切换身份、客户端或连接，应使用 HandleSecretWithCommit，避免保存前产生副作用。
func (s *Service) HandleSecret(key string, apply func(value string) error, opts ...Option) {
	s.declare(key, "", settingSecret, apply, opts...)
}

// HandleSecretWithCommit 将秘密的校验与运行时发布分开，适用于带身份、连接等派生状态的值。
// validate 不得修改运行时状态；失败时不写盘、不调用 commit。
// 保存成功并更新 SettingValue 后才调用 commit；启动加载时同样校验、发布，但不回写文件。
// commit 只做不会失败的本地状态切换，可释放旧连接，但不发起或等待网络请求。
// 可读 SettingValue / SettingSnapshot，不可重入 ApplySetting。
// 两个回调与其他设置变更串行执行。声明必须在 Start 前完成。
func (s *Service) HandleSecretWithCommit(key string, validate func(value string) error, commit func(value string), opts ...Option) {
	opts = append(opts, func(e *settingEntry) { e.commit = commit })
	s.declare(key, "", settingSecret, validate, opts...)
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

// Settings 返回已声明的设置项（注册名片用的同一份声明）。
// 给"服务自检声明是否正确"的调用方与测试用；面板侧走设置中心的 list_settings。
func (s *Service) Settings() []protocol.Setting { return s.declarations() }

// ApplySetting 校验并应用一个设置项——与设置中心下发（configure）走完全同一条路径，
// 供服务自己或测试在不经过中心时验证"这个值能不能被接受"。
func (s *Service) ApplySetting(key, value string) error { return s.applySetting(key, value) }

// SettingValue 返回某设置项最近一次成功应用的生效值（未下发/拉取过时 = 声明预设）。
// 秘密项返回的是**本服务自己**持有的值（中心那边没有）；日志/名片仍不回显。
func (s *Service) SettingValue(key string) string {
	value, _ := s.SettingSnapshot(key)
	return value
}

// SettingSnapshot 在同一把锁内读取生效值与进程内版本，供调用方识别 A→B→A 的变更。
// 仅成功改变生效值时递增；失败或重复保存不变。版本不持久化，不能跨进程比较。
// 秘密值仅供本服务使用，不得写入日志、名片或对外消息。
func (s *Service) SettingSnapshot(key string) (value string, version uint64) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	idx, ok := s.settingIndex[key]
	if !ok {
		return "", 0
	}
	return s.settingValues[key], s.settings[idx].version
}

// 调用方持有 settingsMu；值和版本一起发布，避免读到不属于同一次提交的组合。
func (s *Service) setSettingValueLocked(key, value string) {
	if s.settingValues[key] != value {
		s.settings[s.settingIndex[key]].version++
	}
	s.settingValues[key] = value
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
			Key: it.key, Label: it.label, Hint: it.hint, Kind: it.input, Options: it.options, Unit: it.unit,
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
	var commit func(string)
	kind := settingNormal
	if ok {
		apply = s.settings[idx].apply
		commit = s.settings[idx].commit
		kind = s.settings[idx].kind
	}
	s.settingsMu.Unlock()
	if !ok {
		return fmt.Errorf("configure: 未声明的设置项 %q", key)
	}
	if kind == settingReadOnly {
		return fmt.Errorf("configure: %s 是只读项（启动参数带起来的，改它需要重启）", key)
	}

	// 秘密项：保存失败不能发布新状态；传统回调通过回滚恢复（见 applySecret）。
	if kind == settingSecret {
		return s.applySecret(key, value, apply, commit)
	}
	if apply != nil {
		if err := apply(value); err != nil {
			return fmt.Errorf("configure: %s: %w", key, err)
		}
	}
	s.settingsMu.Lock()
	s.setSettingValueLocked(key, value)
	s.settingsMu.Unlock()
	return nil
}

// applySecret 应用并落盘一个秘密项；落盘失败 = 这次变更没发生：
// 有 commit 时尚未发布运行时状态，无需回滚；传统 apply 则恢复上一个已生效值。
// 调用方已持有 applyMu。
func (s *Service) applySecret(key, value string, apply func(string) error, commit func(string)) error {
	s.settingsMu.Lock()
	previous, known := s.settingValues[key], s.appliedSecrets[key]
	s.settingsMu.Unlock()

	if apply != nil {
		if err := apply(value); err != nil {
			return fmt.Errorf("configure: %s: %w", key, err)
		}
	}
	if err := s.saveSecret(key, value); err != nil {
		// 旧值此前通过过同一个回调；回滚失败只可能是服务自身的 bug——
		// 记日志，但不掩盖原始的落盘错误。
		// 只有骨架确实应用过旧值时才回滚：启动参数可能在骨架之外直接应用过值，
		// 骨架不知道它，贸然回滚会把一个能用的值清掉。
		if commit == nil && known && apply != nil {
			if rollbackErr := apply(previous); rollbackErr != nil {
				s.logger.Printf("秘密 %s 落盘失败后回滚运行时值失败（当前可能仍是新值）: %v", key, rollbackErr)
			}
		}
		return fmt.Errorf("configure: %s: 秘密落盘失败: %w", key, err)
	}
	s.settingsMu.Lock()
	s.setSettingValueLocked(key, value)
	s.appliedSecrets[key] = true
	s.settingsMu.Unlock()
	if commit != nil {
		commit(value)
	}
	return nil
}

// applyLoadedSecret 应用一个从本地秘密文件读回的值：不再回写文件。
// 加载路径以文件为权威副本，重复写盘只会在文件只读时制造假失败。
func (s *Service) applyLoadedSecret(key, value string) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	s.settingsMu.Lock()
	idx, ok := s.settingIndex[key]
	var apply func(string) error
	var commit func(string)
	if ok {
		apply = s.settings[idx].apply
		commit = s.settings[idx].commit
	}
	s.settingsMu.Unlock()
	if !ok {
		return fmt.Errorf("configure: 未声明的设置项 %q", key)
	}
	if apply != nil {
		if err := apply(value); err != nil {
			return fmt.Errorf("configure: %s: %w", key, err)
		}
	}
	s.settingsMu.Lock()
	s.setSettingValueLocked(key, value)
	s.appliedSecrets[key] = true
	s.settingsMu.Unlock()
	if commit != nil {
		commit(value)
	}
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
		if cctx.Err() != nil || s.isClosed() {
			return
		}
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
