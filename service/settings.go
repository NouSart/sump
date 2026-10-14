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

// settingEntry 是一个已声明的设置项。
type settingEntry struct {
	key    string
	preset string
	apply  func(value string) error
}

// HandleSetting 声明一个可设置项：预设值 + 校验/应用回调（必须在 Start 前调用）。
// 预设值由服务自己决定（库不规定来源）；回调返回错误 = 明确拒绝该值，服务保持旧值。
func (s *Service) HandleSetting(key, preset string, apply func(value string) error) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		s.logger.Printf("HandleSetting(%s) 必须在 Start 前调用——已忽略", key)
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if _, dup := s.settingIndex[key]; dup {
		s.logger.Printf("设置项重复声明（忽略）: %s", key)
		return
	}
	s.settingIndex[key] = len(s.settings)
	s.settings = append(s.settings, settingEntry{key: key, preset: preset, apply: apply})
	s.settingValues[key] = preset
}

// SettingValue 返回某设置项最近一次成功应用的生效值（未下发/拉取过时 = 声明预设）。
func (s *Service) SettingValue(key string) string {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.settingValues[key]
}

// declarations 返回名片用的设置声明（按声明顺序）。
func (s *Service) declarations() []protocol.Setting {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	out := make([]protocol.Setting, 0, len(s.settings))
	for _, it := range s.settings {
		out = append(out, protocol.Setting{Key: it.key, Default: it.preset})
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
	s.logger.Printf("设置生效（下发）: %s=%s", p.Key, p.Value)
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
	if ok {
		apply = s.settings[idx].apply
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
