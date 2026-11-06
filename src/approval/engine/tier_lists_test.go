package engine

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
)

// mediumOnlyFileOps 是两份黑名单之间**唯一允许**的差集：
// 「中」档拦、而「工作区」档刻意放行的"在工作区内改文件"的动作。
//
// 这一份差集是设计，不是漏拦：工作区档的全部收益就是"区内的读/写/删/移/复制不打断点击"
// （见 tier_workspace.go 顶部的口径）。所以一个命令名只出现在 mediumBlocked 里是合法的，
// 只要它属于这里；反过来，一个命令名只出现在 workspaceBlocked 里**永远不合法**——
// 那意味着更松的档位拦住了系统级动作，更严的档位却自动放行（就是这次修掉的倒挂）。
//
// 往 workspaceBlocked 或 mediumBlocked 里**新增命令**时，必须在这里做一次归类，二选一：
//  1. 它是区内文件操作（工作区档放行是设计意图）→ 加进本表，并写明为什么；
//  2. 它是系统级动作（提权 / 装软件 / 外发拷贝 / 网络扫描 / 改账号 / 改系统状态 / 终止进程）
//     → 加进 mediumBlocked，本表不动。
//
// 底下 TestTierListsNoInversion 会把这几件事全部机器校验一遍，不再靠两份注释互相承诺
// ——历史上两边各写各的注释，就是这么漂移到"medium 比 workspace 还松"的。
var mediumOnlyFileOps = map[string]string{
	"rm":       "删除文件",
	"rmdir":    "删除目录",
	"ri":       "删除文件",
	"del":      "删除文件",
	"erase":    "删除文件",
	"rd":       "删除目录",
	"shred":    "彻底删除文件",
	"truncate": "截断文件",
	"mv":       "移动或覆盖文件",
	"cp":       "复制并覆盖文件",
	"install":  "安装文件",
	"chmod":    "改文件权限",
	"chown":    "改文件归属",
	"chgrp":    "改文件归属",
	"attrib":   "改文件属性",
}

// tierReasonSynonyms 把两份名单对同一类动作的不同措辞并到一起：
// 这里校验的是"类别有没有被中档覆盖"，不是"字符串相等"，两边各自用词不同是允许的。
var tierReasonSynonyms = map[string]string{
	"联网外发":  "联网取送文件",
	"装系统软件": "装软件",
}

// tierListMissing 返回 wanted 里有、base 里却没有的命令名（按字典序，报错文案才稳定）。
func tierListMissing(base, wanted map[string]string) []string {
	var out []string
	for name := range wanted {
		if _, ok := base[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// tierReasonCategory 归一化理由措辞，用于类别层面的比较。
func tierReasonCategory(why string) string {
	if synced, ok := tierReasonSynonyms[why]; ok {
		return synced
	}
	return why
}

// TestTierListsNoInversion：档位不能倒挂。
//
// 「中」档（听起来更严）拦的命令名必须是「工作区」档（设计上更松）的超集；
// 两份名单的差集只能落在 mediumOnlyFileOps 那一类"区内文件操作"上。
func TestTierListsNoInversion(t *testing.T) {
	// 两份名单的差集，方向不同含义完全不同，所以各算一次、各自断言：
	//   workspaceOnly —— 只被更松的档位拦着，这就是倒挂，必须为空；
	//   mediumOnly    —— 只被更严的档位拦着，只允许是「区内文件操作」那一类。
	// 两个变量只被下面的断言用到：删掉任何一条断言，这个文件就编译不过（差集不会被"顺手删了"）。
	workspaceOnly := tierListMissing(mediumBlocked, workspaceBlocked)
	mediumOnly := tierListMissing(workspaceBlocked, mediumBlocked)
	// ① 系统级动作不许只被更松的档位拦着。
	for _, name := range workspaceOnly {
		t.Errorf("档位倒挂：命令 %q 在 workspaceBlocked 里被拦（理由：%s），却不在 mediumBlocked 里——"+
			"更松的「工作区」档拦住了系统级动作，更严的「中」档反而自动放行。"+
			"要么把 %q 加进 mediumBlocked 并写明它属于哪一类（提权 / 装软件 / 外发拷贝 / 网络扫描 / 改账号 / 改系统状态 / 终止进程），"+
			"要么确认它是「工作区内的文件操作」并加进 mediumOnlyFileOps 说明为什么工作区档该放行它。"+
			"（涉及变量：mediumBlocked、workspaceBlocked）", name, workspaceBlocked[name], name)
	}
	// ② 中档多拦的部分必须被显式归类，不能凭"反正中档严"就多加。
	for _, name := range mediumOnly {
		if _, ok := mediumOnlyFileOps[name]; !ok {
			t.Errorf("命令 %q 只在 mediumBlocked 里（理由：%s）、不在 workspaceBlocked 里，却没在 mediumOnlyFileOps 里归类："+
				"只有「工作区内的文件操作」允许只出现在中档名单里；如果它是系统级动作，请同时加进 workspaceBlocked，"+
				"否则请把它加进 mediumOnlyFileOps 并写明为什么工作区档要放行它。（涉及变量：mediumBlocked、workspaceBlocked）",
				name, mediumBlocked[name])
		}
	}
	// ③ 白名单不能变成化石：名单一旦改了，这里必须同步。
	for name := range mediumOnlyFileOps {
		if _, ok := mediumBlocked[name]; !ok {
			t.Errorf("mediumOnlyFileOps 里的 %q 已经不在 mediumBlocked 里了：中档不再拦它，白名单要同步删掉。（涉及变量：mediumBlocked）", name)
		}
		if _, ok := workspaceBlocked[name]; ok {
			t.Errorf("mediumOnlyFileOps 里的 %q 现在也出现在 workspaceBlocked 里，已经不属于两份名单的差集：白名单要同步删掉。（涉及变量：workspaceBlocked）", name)
		}
	}
	// ④ 中档要认得工作区档用到的每一类动作：类别层面的覆盖（措辞不同不算不同类）。
	mediumCategories := make(map[string]bool, len(mediumBlocked))
	for _, why := range mediumBlocked {
		mediumCategories[tierReasonCategory(why)] = true
	}
	var lost []string
	for _, why := range workspaceBlocked {
		if !mediumCategories[tierReasonCategory(why)] {
			lost = append(lost, why)
		}
	}
	if lost = dedupe(lost); len(lost) > 0 {
		sort.Strings(lost)
		t.Errorf("工作区档用到的动作类别 %v 在 mediumBlocked 的理由里一个都没有：中档没有覆盖这一类动作，"+
			"要么把这一类命令补进 mediumBlocked，要么在 tierReasonSynonyms 里说明它和中档哪一类是同义词。（涉及变量：mediumBlocked、workspaceBlocked）", lost)
	}
}

// listTierEngine 造一个指定档位的引擎；档位写错直接让用例失败，不许悄悄回落。
func listTierEngine(t *testing.T, tier string) *Engine {
	t.Helper()
	e, err := New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateTier(tier); err != nil {
		t.Fatal(err)
	}
	return e
}

// listBashRequest 构造一条 POSIX shell 的执行描述。
//
// WorkDir 用平台真实存在的绝对路径：Windows 上的 C:\work 在 Linux 上不是绝对路径，
// 会让判定退化成 CONTEXT_UNKNOWN（和 bash_test.go 里的 bashRequest 同一个理由）。
func listBashRequest(command string) protocol.CommandApprovalRequest {
	shell := "/bin/bash"
	if runtime.GOOS == "windows" {
		shell = `C:\Program Files\Git\bin\bash.exe`
	}
	return protocol.CommandApprovalRequest{Version: 1, ItemID: "tier-lists", Plan: protocol.ExecutionPlan{
		Version: 1, Command: command, WorkDir: os.TempDir(), ShellPath: shell, Args: []string{"-c"},
		Dialect: "bash", Platform: runtime.GOOS, Profile: "unsupported", TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
}

// TestTierSameNameSameTreatment：同一个命令名在两份名单里就得是同一套待遇。
//
// 审计的抱怨是"同一类事件两套待遇"：这里把两档共有的每一个命令名都跑一遍，
// 要求决策与理由码逐字相同，而且都是"转人工"——不是放行，也不是硬拒
// （硬拒只属于 policy 的 deny_commands）。
func TestTierSameNameSameTreatment(t *testing.T) {
	medium := listTierEngine(t, "medium")
	workspace := listTierEngine(t, "workspace")
	var shared []string
	for name := range mediumBlocked {
		if _, ok := workspaceBlocked[name]; ok {
			shared = append(shared, name)
		}
	}
	if len(shared) == 0 {
		t.Fatal("两份名单没有交集，共有的系统级动作不可能一个都不拦：说明名单被清空了")
	}
	sort.Strings(shared)
	snapshot, err := policy.Load(policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shared {
		cmd := name + " something"
		gm, err := medium.Evaluate(context.Background(), listBashRequest(cmd))
		if err != nil {
			t.Fatalf("%s（中档）：%v", cmd, err)
		}
		gw, err := workspace.Evaluate(context.Background(), listBashRequest(cmd))
		if err != nil {
			t.Fatalf("%s（工作区档）：%v", cmd, err)
		}
		if gm.Decision != gw.Decision || gm.ReasonCode != gw.ReasonCode {
			t.Errorf("%q 一套事件两套待遇：中档 %s/%s，工作区档 %s/%s（涉及变量：mediumBlocked、workspaceBlocked）",
				name, gm.Decision, gm.ReasonCode, gw.Decision, gw.ReasonCode)
		}
		// deny_commands 优先于任何档位（见 policy.DefaultJSON），这类名字两档都是硬拒；
		// 其余名单里的名字只能是转人工。
		want := protocol.DecisionReview
		if snapshot.Denied(name) {
			want = protocol.DecisionDeny
		}
		if gm.Decision != want {
			t.Errorf("%q 应是 %s，中档实际 %s/%s（黑名单的含义是「不自动放行」，不是硬拒）", name, want, gm.Decision, gm.ReasonCode)
		}
	}
}

// TestTierWorkspaceVerbsNeverAutoPassInMedium：名单层面的超集，运行时也要成立。
//
// 这是给"改了 map 但判定没接上"留的一道：workspaceBlocked 里的每个名字，
// 在中档都不许被自动放行。
func TestTierWorkspaceVerbsNeverAutoPassInMedium(t *testing.T) {
	medium := listTierEngine(t, "medium")
	for name := range workspaceBlocked {
		got, err := medium.Evaluate(context.Background(), listBashRequest(name+" something"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Decision == protocol.DecisionAllow {
			t.Errorf("%q 在工作区档被拦（理由：%s），中档却自动放行了——档位倒挂。（涉及变量：mediumBlocked、workspaceBlocked）",
				name, workspaceBlocked[name])
		}
		medWhy, ok := mediumBlocked[name]
		if !ok {
			continue // 名单层面的倒挂由 TestTierListsNoInversion 报，这里不重复判
		}
		if !strings.Contains(got.Feedback.Summary, medWhy) {
			t.Errorf("%q 在工作区档被拦（理由：%s），中档卡片却说不出自己的理由（期望包含 %q，实际：%s）",
				name, workspaceBlocked[name], medWhy, got.Feedback.Summary)
		}
	}
}

// TestTierEdgesUnknownTierAndEmptyInputs：错误路径——档位写错、名单为空、命令为空。
func TestTierEdgesUnknownTierAndEmptyInputs(t *testing.T) {
	// 档位名只认四个：认不出来必须报错，并保持原档位，不能默默当成"中"或"工作区"。
	e := listTierEngine(t, "medium")
	for _, bad := range []string{"", " ", "mid", "Middle", "workspace2", "auto", "1"} {
		if err := e.UpdateTier(bad); err == nil {
			t.Errorf("档位 %q 应被拒绝，却通过了（可选：off / low / medium / workspace）", bad)
		}
	}
	if e.Tier() != TierMedium {
		t.Fatalf("非法档位不应改掉当前档位，实际 %s", e.Tier())
	}
	// 大小写与首尾空格是同一档：别把 " MEDIUM " 判成两次错误。
	if got, err := ParseTier(" MEDIUM "); err != nil || got != TierMedium {
		t.Errorf(`" MEDIUM " 应解析为中档，实际 %q/%v`, got, err)
	}

	// 空命令文本：必须显式报错，绝不能"没有命令 = 没有风险 = 放行"。
	for _, tier := range []string{"off", "low", "medium", "workspace"} {
		engine := listTierEngine(t, tier)
		for _, cmd := range []string{"", "   ", "\t"} {
			got, err := engine.Evaluate(context.Background(), listBashRequest(cmd))
			if err == nil {
				t.Errorf("%s 档的空命令 %q 应报错，实际 %s/%s", tier, cmd, got.Decision, got.ReasonCode)
				continue
			}
			if got.Decision == protocol.DecisionAllow {
				t.Errorf("%s 档的空命令 %q 不应自动放行", tier, cmd)
			}
		}
	}

	// 名单为空的边界：判定函数不能崩、也不能把"名单空"当成"全都拦"或"全都放"。
	if diff := tierListMissing(map[string]string{}, map[string]string{}); len(diff) != 0 {
		t.Errorf("两份名单都空时差集应为空，实际 %v", diff)
	}
	if diff := tierListMissing(map[string]string{}, workspaceBlocked); len(diff) != len(workspaceBlocked) {
		t.Errorf("mediumBlocked 为空时，workspaceBlocked 的每个名字都该被判成倒挂，实际只报了 %d/%d", len(diff), len(workspaceBlocked))
	}
	if diff := tierListMissing(workspaceBlocked, map[string]string{}); len(diff) != 0 {
		t.Errorf("workspaceBlocked 为空时不该有任何差集，实际 %v", diff)
	}
}
