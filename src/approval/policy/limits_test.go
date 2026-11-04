// 规则包容量上限的边界测试。
//
// `Load` 里有 7 条「允许面」契约（规则条数、拒绝表容量、ID/说明/版本长度、别名宽度、
// 整个规则包字节数）。异变测试把它们的 `>` 改成 `>=` 后**全部存活** —— 说明这些上限
// 一条边界用例都没有。唯一的例外 `MaxArgs > 32` 看似被杀，其实是 `default.json` 里
// 恰好有一条 `max_args: 32` 的规则撞上了边界，纯属巧合。
//
// 每个上限都成对测：恰好等于上限（必须通过）+ 上限加一（必须拒绝）。
// 数值取的是此刻代码里的值 —— 它们是要被钉住的契约，不是随手写的数字；要改就得改这里。
package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// rule 造一条合法规则（ID 唯一），可再改写字段。
func rule(id string) Rule {
	return Rule{ID: id, Names: []string{"Get-Process"}, MaxArgs: 0, Reason: "允许只读命令"}
}

func cfgJSON(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func validConfig() Config {
	return Config{Version: "1", Rules: []Rule{rule("r1")}}
}

func TestLoadAcceptsMinimalConfig(t *testing.T) {
	if _, err := Load(cfgJSON(t, validConfig())); err != nil {
		t.Fatalf("最小合法配置应通过: %v", err)
	}
}

// 规则包整体字节数：64 KiB 恰好通过，多一个字节拒绝。
func TestLoadPackageSizeLimit(t *testing.T) {
	base := cfgJSON(t, validConfig())
	pad := 64<<10 - len(base)
	if pad < 0 {
		t.Fatalf("基础配置已经超过 64 KiB: %d", len(base))
	}
	// 用空格补齐到恰好 64 KiB（JSON 允许尾随空白）。
	exact := base + strings.Repeat(" ", pad)
	if len(exact) != 64<<10 {
		t.Fatalf("构造长度不对: %d", len(exact))
	}
	if _, err := Load(exact); err != nil {
		t.Fatalf("恰好 64 KiB 应通过: %v", err)
	}
	if _, err := Load(exact + " "); err == nil {
		t.Fatal("64 KiB + 1 字节应拒绝")
	}
}

// 规则条数：64 恰好通过，65 拒绝。
func TestLoadRulesCountLimit(t *testing.T) {
	for _, tc := range []struct {
		n      int
		reject bool
	}{{n: 64}, {n: 65, reject: true}} {
		cfg := Config{Version: "1"}
		for i := 0; i < tc.n; i++ {
			r := rule(fmt.Sprintf("r%d", i))
			r.Names = []string{fmt.Sprintf("Cmd-%d", i)} // 别名不能重复
			cfg.Rules = append(cfg.Rules, r)
		}
		_, err := Load(cfgJSON(t, cfg))
		if tc.reject && err == nil {
			t.Fatalf("%d 条规则应拒绝", tc.n)
		}
		if !tc.reject && err != nil {
			t.Fatalf("%d 条规则应通过: %v", tc.n, err)
		}
	}
}

// 拒绝表容量：128 恰好通过，129 拒绝。
func TestLoadDenyCommandsLimit(t *testing.T) {
	for _, tc := range []struct {
		n      int
		reject bool
	}{{n: 128}, {n: 129, reject: true}} {
		cfg := validConfig()
		for i := 0; i < tc.n; i++ {
			cfg.DenyCommands = append(cfg.DenyCommands, fmt.Sprintf("Deny-%d", i))
		}
		_, err := Load(cfgJSON(t, cfg))
		if tc.reject && err == nil {
			t.Fatalf("%d 条拒绝命令应拒绝", tc.n)
		}
		if !tc.reject && err != nil {
			t.Fatalf("%d 条拒绝命令应通过: %v", tc.n, err)
		}
	}
}

// 单条规则的别名宽度：16 恰好通过，17 拒绝。
func TestLoadRuleNamesLimit(t *testing.T) {
	for _, tc := range []struct {
		n      int
		reject bool
	}{{n: 16}, {n: 17, reject: true}} {
		cfg := validConfig()
		cfg.Rules[0].Names = nil
		for i := 0; i < tc.n; i++ {
			cfg.Rules[0].Names = append(cfg.Rules[0].Names, fmt.Sprintf("Cmd-%d", i))
		}
		_, err := Load(cfgJSON(t, cfg))
		if tc.reject && err == nil {
			t.Fatalf("别名 %d 个应拒绝", tc.n)
		}
		if !tc.reject && err != nil {
			t.Fatalf("别名 %d 个应通过: %v", tc.n, err)
		}
	}
}

// 字符串字段长度：恰好等于上限通过，加一拒绝。
func TestLoadStringFieldLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		set   func(*Config, string)
	}{
		{name: "版本", limit: 80, set: func(c *Config, v string) { c.Version = v }},
		{name: "规则 ID", limit: 80, set: func(c *Config, v string) { c.Rules[0].ID = v }},
		{name: "规则说明", limit: 512, set: func(c *Config, v string) { c.Rules[0].Reason = v }},
	} {
		t.Run(tc.name+"/恰好上限", func(t *testing.T) {
			cfg := validConfig()
			tc.set(&cfg, strings.Repeat("a", tc.limit))
			if _, err := Load(cfgJSON(t, cfg)); err != nil {
				t.Fatalf("%s 恰好 %d 应通过: %v", tc.name, tc.limit, err)
			}
		})
		t.Run(tc.name+"/超一个", func(t *testing.T) {
			cfg := validConfig()
			tc.set(&cfg, strings.Repeat("a", tc.limit+1))
			if _, err := Load(cfgJSON(t, cfg)); err == nil {
				t.Fatalf("%s 超过 %d 应拒绝", tc.name, tc.limit)
			}
		})
	}
}

// 参数上限：0 与 32 通过，33 拒绝（这条原本只被 default.json 的巧合覆盖）。
func TestLoadMaxArgsLimit(t *testing.T) {
	for _, tc := range []struct {
		v      int
		reject bool
	}{{v: 0}, {v: 32}, {v: 33, reject: true}, {v: -1, reject: true}} {
		cfg := validConfig()
		cfg.Rules[0].MaxArgs = tc.v
		_, err := Load(cfgJSON(t, cfg))
		if tc.reject && err == nil {
			t.Fatalf("max_args=%d 应拒绝", tc.v)
		}
		if !tc.reject && err != nil {
			t.Fatalf("max_args=%d 应通过: %v", tc.v, err)
		}
	}
}
