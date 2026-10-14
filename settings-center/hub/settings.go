package hub

// 设置保管：各服务注册时声明自己的可设置项（默认值），调用方（前端 / 其他服务）
// 通过设置中心的动作写入覆盖值。生效值 = 覆盖值 ?? 声明默认值。
// 只有覆盖值落盘——默认值随服务的声明变化，不重复保存。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// Settings 保管各服务的可设置项与覆盖值。
type Settings struct {
	mu        sync.Mutex
	declared  map[string][]protocol.Setting // 服务 → 注册时声明的设置项
	overrides map[string]map[string]string  // 服务 → key → 覆盖值
	path      string                        // 覆盖值文件路径（空 = 不落盘）
	log       *log.Logger
}

// settingsFile 是落盘结构。
type settingsFile struct {
	Version   int                          `json:"version"`
	Overrides map[string]map[string]string `json:"overrides"`
}

// NewSettings 创建设置保管：path 为覆盖值文件（不存在视为空；path 为空则不落盘）。
func NewSettings(path string, logger *log.Logger) (*Settings, error) {
	s := &Settings{
		declared:  map[string][]protocol.Setting{},
		overrides: map[string]map[string]string{},
		path:      path,
		log:       logger,
	}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("读取设置文件失败: %w", err)
	}
	var f settingsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析设置文件失败: %w", err)
	}
	for svc, kv := range f.Overrides {
		s.overrides[svc] = kv
	}
	if n := len(s.overrides); n > 0 && logger != nil {
		logger.Printf("已载入设置覆盖: %d 个服务（%s）", n, path)
	}
	return s, nil
}

// Declare 记录某服务声明的设置项（注册时调用）。
// 未再声明的项其覆盖值会被清除——保管内容始终与服务声明一致。
// 返回声明是否发生变化（含清理覆盖；调用方据此决定是否广播 settings 事件）。
func (s *Settings) Declare(service string, items []protocol.Setting) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.declared[service]
	changed := !sameSettings(old, items)
	s.declared[service] = items

	ov := s.overrides[service]
	if ov == nil {
		return changed
	}
	valid := make(map[string]bool, len(items))
	for _, it := range items {
		valid[it.Key] = true
	}
	dropped := false
	for k := range ov {
		if !valid[k] {
			delete(ov, k)
			dropped = true
		}
	}
	if len(ov) == 0 {
		delete(s.overrides, service)
	}
	if dropped {
		s.logf("服务 %s 重新注册：清除未再声明的设置覆盖", service)
		if err := s.saveLocked(); err != nil {
			s.logf("保存设置失败: %v", err)
		}
		changed = true
	}
	return changed
}

// Items 返回某服务的生效值清单（未声明返回 nil）。
func (s *Settings) Items(service string) []protocol.SettingView {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.viewLocked(service)
	if !ok {
		return nil
	}
	return ss.Settings
}

// sameSettings 比较两份设置声明（nil 与空视为相同）。
func sameSettings(a, b []protocol.Setting) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// List 返回设置清单：service 为空 = 全部（只含声明了设置项的服务，按服务名排序）。
func (s *Settings) List(service string) []protocol.ServiceSettings {
	s.mu.Lock()
	defer s.mu.Unlock()

	var names []string
	if service != "" {
		if _, ok := s.declared[service]; !ok {
			return nil
		}
		names = []string{service}
	} else {
		for n, items := range s.declared {
			if len(items) > 0 {
				names = append(names, n)
			}
		}
		sort.Strings(names)
	}

	out := make([]protocol.ServiceSettings, 0, len(names))
	for _, n := range names {
		if ss, ok := s.viewLocked(n); ok {
			out = append(out, ss)
		}
	}
	return out
}

// Set 写入覆盖值（服务与设置项都必须已声明），返回该服务的最新设置。
func (s *Settings) Set(service, key, value string) (protocol.ServiceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(service, key); err != nil {
		return protocol.ServiceSettings{}, err
	}
	if s.overrides[service] == nil {
		s.overrides[service] = map[string]string{}
	}
	s.overrides[service][key] = value
	if err := s.saveLocked(); err != nil {
		return protocol.ServiceSettings{}, err
	}
	ss, _ := s.viewLocked(service)
	return ss, nil
}

// Reset 清除覆盖值，回落服务声明的默认值，返回该服务的最新设置。
func (s *Settings) Reset(service, key string) (protocol.ServiceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(service, key); err != nil {
		return protocol.ServiceSettings{}, err
	}
	if ov := s.overrides[service]; ov != nil {
		delete(ov, key)
		if len(ov) == 0 {
			delete(s.overrides, service)
		}
		if err := s.saveLocked(); err != nil {
			return protocol.ServiceSettings{}, err
		}
	}
	ss, _ := s.viewLocked(service)
	return ss, nil
}

// checkLocked 校验服务已注册且该项已被声明。
func (s *Settings) checkLocked(service, key string) error {
	items, ok := s.declared[service]
	if !ok {
		return fmt.Errorf("未注册的服务: %s", service)
	}
	for _, it := range items {
		if it.Key == key {
			return nil
		}
	}
	return fmt.Errorf("服务 %s 未声明设置项: %s", service, key)
}

// viewLocked 组装某服务的设置视图（保留声明顺序）。
func (s *Settings) viewLocked(service string) (protocol.ServiceSettings, bool) {
	items := s.declared[service]
	if len(items) == 0 {
		return protocol.ServiceSettings{}, false
	}
	ov := s.overrides[service]
	views := make([]protocol.SettingView, 0, len(items))
	for _, it := range items {
		v := it.Default
		overridden := false
		if got, ok := ov[it.Key]; ok {
			v = got
			overridden = true
		}
		views = append(views, protocol.SettingView{
			Key: it.Key, Value: v, Default: it.Default, Overridden: overridden,
		})
	}
	return protocol.ServiceSettings{Service: service, Settings: views}, true
}

// saveLocked 原子落盘：先写临时文件再改名，避免半个文件。
func (s *Settings) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(settingsFile{Version: 1, Overrides: s.overrides}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Settings) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}
