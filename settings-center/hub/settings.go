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
	declared  map[string][]protocol.Setting       // 服务 → 注册时声明的设置项
	overrides map[string]map[string]string        // 服务 → key → 覆盖值
	reported  map[string]map[string]reportedValue // 服务 → key → 服务侧自报的实际值（运行时态，不落盘）
	path      string                              // 覆盖值文件路径（空 = 不落盘）
	log       *log.Logger
}

// reportedValue 是服务自报的实际值 + 它的单调序号（乱序旧报据此丢弃）。
type reportedValue struct {
	value string
	seq   int64
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
		reported:  map[string]map[string]reportedValue{},
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
	valid := make(map[string]bool, len(items))
	for _, it := range items {
		valid[it.Key] = true
	}
	// 未再声明的项：服务侧自报值也一并清掉（键可能被重新定义成别的东西）
	if rep := s.reported[service]; rep != nil {
		for k := range rep {
			if !valid[k] {
				delete(rep, k)
			}
		}
		if len(rep) == 0 {
			delete(s.reported, service)
		}
	}
	if ov == nil {
		return changed
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

// PushItems 返回注册对齐时该下发的项：只含普通项——
// 秘密项中心手里根本没有值（推个空过去只会把服务自己保存的那份抹掉），
// 只读项是启动参数带起来的（推过去也只会被服务拒一次，纯噪声）。
func (s *Settings) PushItems(service string) []protocol.SettingView {
	views := s.Items(service)
	out := make([]protocol.SettingView, 0, len(views))
	for _, v := range views {
		if !v.Secret && !v.ReadOnly {
			out = append(out, v)
		}
	}
	return out
}

// sameSettings 比较两份设置声明（nil 与空视为相同）。
// Options 是切片，所以逐字段比而不是整体比较（结构体已不再可比较）。
func sameSettings(a, b []protocol.Setting) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Key != y.Key || x.Default != y.Default || x.Secret != y.Secret ||
			x.ReadOnly != y.ReadOnly || x.Set != y.Set ||
			x.Label != y.Label || x.Hint != y.Hint || x.Kind != y.Kind || x.Unit != y.Unit {
			return false
		}
		if len(x.Options) != len(y.Options) {
			return false
		}
		for j := range x.Options {
			if x.Options[j] != y.Options[j] {
				return false
			}
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
// 秘密项**不落盘**：值只作为一次性下发参数交给服务（中心这边不留），谁消费谁持有；
// 只读项直接拒绝——它的值来自启动参数，改它要重启。
func (s *Settings) Set(service, key, value string) (protocol.ServiceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.findLocked(service, key)
	if err != nil {
		return protocol.ServiceSettings{}, err
	}
	if it.ReadOnly {
		return protocol.ServiceSettings{}, fmt.Errorf("%s.%s 是只读项（启动参数带起来的，改它需要重启）", service, key)
	}
	if !it.Secret {
		if s.overrides[service] == nil {
			s.overrides[service] = map[string]string{}
		}
		s.overrides[service][key] = value
		if err := s.saveLocked(); err != nil {
			return protocol.ServiceSettings{}, err
		}
	}
	ss, _ := s.viewLocked(service)
	return ss, nil
}

// Reset 清除覆盖值，回落服务声明的默认值，返回该服务的最新设置。
// 秘密项没有覆盖值可清：中心什么都不做，清空动作由调用方把空值推给服务（服务自己清本地那份）。
func (s *Settings) Reset(service, key string) (protocol.ServiceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.findLocked(service, key)
	if err != nil {
		return protocol.ServiceSettings{}, err
	}
	if it.ReadOnly {
		return protocol.ServiceSettings{}, fmt.Errorf("%s.%s 是只读项（启动参数带起来的，改它需要重启）", service, key)
	}
	if !it.Secret {
		if ov := s.overrides[service]; ov != nil {
			delete(ov, key)
			if len(ov) == 0 {
				delete(s.overrides, service)
			}
			if err := s.saveLocked(); err != nil {
				return protocol.ServiceSettings{}, err
			}
		}
	}
	ss, _ := s.viewLocked(service)
	return ss, nil
}

// MarkSecretSet 更新某秘密项的「服务侧有没有值」标记（下发成功后由调用方调）。
// 只改内存里的声明快照——真相在服务自己那边，服务下次注册会带着真实状态回来。
func (s *Settings) MarkSecretSet(service, key string, set bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markSecretSetLocked(service, key, set)
}

// markSecretSetLocked 同上，调用方已持锁。
func (s *Settings) markSecretSetLocked(service, key string, set bool) {
	items := s.declared[service]
	for i := range items {
		if items[i].Key == key && items[i].Secret {
			items[i].Set = set
			return
		}
	}
}

// NotePushed 记下「这一跳已经把值送达并生效」：服务侧实际值即该值，面板不必报「服务侧已变」。
// 只改值，**保留**已经记下的报告水位（那是服务侧序号的比较基准，抹掉会让旧报被错收）。
func (s *Settings) NotePushed(service, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.findLocked(service, key)
	if err != nil || it.Secret || it.ReadOnly {
		return
	}
	rep := s.reported[service]
	if rep == nil {
		rep = map[string]reportedValue{}
		s.reported[service] = rep
	}
	rep[key] = reportedValue{value: value, seq: rep[key].seq}
}

// Report 记下一次「服务自报的实际值」（服务侧自己变了）：只更新服务侧那一列，
// **不动**中心手里的覆盖值（谁的值是谁的）。秘密项只认「有没有值」，值永不上线。
// seq 是服务内单调序号：比已记下的小 = 乱序旧报，丢弃。
func (s *Settings) Report(service, key, value string, set bool, seq int64) (protocol.ServiceSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.findLocked(service, key)
	if err != nil {
		return protocol.ServiceSettings{}, err
	}
	if it.Secret {
		s.markSecretSetLocked(service, key, set)
	} else {
		rep := s.reported[service]
		if rep == nil {
			rep = map[string]reportedValue{}
			s.reported[service] = rep
		}
		if old, ok := rep[key]; ok && seq != 0 && seq < old.seq {
			ss, _ := s.viewLocked(service)
			return ss, nil
		}
		rep[key] = reportedValue{value: value, seq: seq}
	}
	ss, _ := s.viewLocked(service)
	return ss, nil
}

// findLocked 校验服务已注册且该项已被声明，并返回该声明。
func (s *Settings) findLocked(service, key string) (protocol.Setting, error) {
	items, ok := s.declared[service]
	if !ok {
		return protocol.Setting{}, fmt.Errorf("未注册的服务: %s", service)
	}
	for _, it := range items {
		if it.Key == key {
			return it, nil
		}
	}
	return protocol.Setting{}, fmt.Errorf("服务 %s 未声明设置项: %s", service, key)
}

// viewLocked 组装某服务的设置视图（保留声明顺序）。
// 秘密项只回「有没有值」（值永不上线）；只读项回启动值并标注不可改。
func (s *Settings) viewLocked(service string) (protocol.ServiceSettings, bool) {
	items := s.declared[service]
	if len(items) == 0 {
		return protocol.ServiceSettings{}, false
	}
	ov := s.overrides[service]
	rep := s.reported[service]
	views := make([]protocol.SettingView, 0, len(items))
	for _, it := range items {
		// 人话元数据跟着声明走（服务说它叫什么、怎么解释、用什么控件）。
		human := protocol.SettingView{Label: it.Label, Hint: it.Hint, Kind: it.Kind, Options: it.Options, Unit: it.Unit}
		switch {
		case it.Secret:
			v := human
			v.Key, v.Secret, v.Set = it.Key, true, it.Set
			views = append(views, v)
		case it.ReadOnly:
			rv, ok := rep[it.Key]
			v := human
			v.Key, v.Value, v.Default, v.ReadOnly = it.Key, it.Default, it.Default, true
			v.Actual, v.Reported = rv.value, ok
			views = append(views, v)
		default:
			v := human
			value := it.Default
			overridden := false
			if got, ok := ov[it.Key]; ok {
				value = got
				overridden = true
			}
			rv, ok := rep[it.Key]
			v.Key, v.Value, v.Default, v.Overridden = it.Key, value, it.Default, overridden
			v.Actual, v.Reported = rv.value, ok
			views = append(views, v)
		}
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
