// Package policy 保存不可变规则快照。配置只声明静态命令规则，不能携带可执行代码。
package policy

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

//go:embed default.json
var DefaultJSON string

type Rule struct {
	ID      string   `json:"id"`
	Names   []string `json:"names"`
	MaxArgs int      `json:"max_args"`
	Reason  string   `json:"reason"`
}

type Config struct {
	Version      string   `json:"version"`
	DenyCommands []string `json:"deny_commands"`
	Rules        []Rule   `json:"rules"`
}

type Snapshot struct {
	Version string
	deny    map[string]bool
	allow   map[string]Rule
}

var commandName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,79}$`)

func Load(text string) (*Snapshot, error) {
	if len(text) > 64<<10 {
		return nil, errors.New("规则包超过 64 KiB")
	}
	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields() // 配置格式不是通用消息信封；拼错设置不能静默忽略。
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("规则配置无效: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("规则包后存在多余或无效内容")
	}
	if cfg.Version == "" || len(cfg.Version) > 80 || len(cfg.Rules) > 64 || len(cfg.DenyCommands) > 128 {
		return nil, errors.New("规则版本为空或规则数量超限")
	}
	s := &Snapshot{deny: map[string]bool{}, allow: map[string]Rule{}}
	ids := map[string]bool{}
	for _, name := range cfg.DenyCommands {
		if !commandName.MatchString(name) {
			return nil, errors.New("拒绝命令名称无效")
		}
		s.deny[strings.ToLower(name)] = true
	}
	for _, rule := range cfg.Rules {
		if rule.ID == "" || len(rule.ID) > 80 || ids[rule.ID] || rule.MaxArgs < 0 || rule.MaxArgs > 32 || len(rule.Names) == 0 || len(rule.Names) > 16 || rule.Reason == "" || len(rule.Reason) > 512 {
			return nil, errors.New("规则 ID、参数范围或说明无效")
		}
		ids[rule.ID] = true
		for _, name := range rule.Names {
			if !commandName.MatchString(name) {
				return nil, errors.New("允许命令名称无效")
			}
			key := strings.ToLower(name)
			if _, exists := s.allow[key]; exists {
				return nil, errors.New("重复的命令规则")
			}
			s.allow[key] = rule
		}
	}
	sum := sha256.Sum256([]byte(text))
	s.Version = cfg.Version + ":" + hex.EncodeToString(sum[:8])
	return s, nil
}

func (s *Snapshot) Denied(name string) bool { return s.deny[strings.ToLower(name)] }
func (s *Snapshot) Rule(name string) (Rule, bool) {
	r, ok := s.allow[strings.ToLower(name)]
	return r, ok
}
