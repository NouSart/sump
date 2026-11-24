package assemble

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- 测试夹具 ----------

func testInput() Input {
	return Input{
		WorkID:         "w1",
		TaskID:         "t1",
		TaskName:       "修登录",
		ConversationID: "c1",
		Owner:          "u1",
		Entry:          "web",
		Config:         DefaultConfig(),
		Now:            time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
	}
}

func mustAssemble(t *testing.T, in Input) Output {
	t.Helper()
	out, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble 返回错误（纯函数不应因依赖缺失失败）: %v", err)
	}
	return out
}

// cloneInput 深拷贝一份输入，用来验证 Assemble 不改入参。
func cloneInput(in Input) Input {
	out := in
	out.History = append([]Record(nil), in.History...)
	out.ToolCalls = append([]Record(nil), in.ToolCalls...)
	out.ToolResults = append([]Record(nil), in.ToolResults...)
	out.Inflight = append([]Inflight(nil), in.Inflight...)
	out.Memories = append([]MemoryEntry(nil), in.Memories...)
	out.Experience = append([]LineHead(nil), in.Experience...)
	out.Shared = make([]SessionDigest, len(in.Shared))
	for i, s := range in.Shared {
		out.Shared[i] = s
		out.Shared[i].Capabilities = append([]string(nil), s.Capabilities...)
	}
	out.Grants = append([]Grant(nil), in.Grants...)
	out.Images = append([]ImageRef(nil), in.Images...)
	if in.Degraded != nil {
		d := *in.Degraded
		out.Degraded = &d
	}
	return out
}

func findProv(out Output, source string) []Provenance {
	var got []Provenance
	for _, p := range out.Provenance {
		if p.Source == source {
			got = append(got, p)
		}
	}
	return got
}

func hasWarning(out Output, substr string) bool {
	for _, w := range out.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// ---------- 空输入与 system ----------

func TestEmptyInputOnlyHasSystem(t *testing.T) {
	out := mustAssemble(t, Input{})
	if len(out.Messages) != 1 {
		t.Fatalf("空输入应只有一条 system，得到 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("首条消息必须是 system，得到 %q", out.Messages[0].Role)
	}
	if out.Messages[0].Content != "" {
		t.Fatalf("空输入的系统提示应为空（可接受空），得到 %q", out.Messages[0].Content)
	}
	if len(out.Warnings) != 0 || len(out.Provenance) != 0 {
		t.Fatalf("空输入不应产生 warning/provenance: %+v / %+v", out.Warnings, out.Provenance)
	}
	if out.Degraded != nil {
		t.Fatalf("空输入没有 degraded，得到 %+v", out.Degraded)
	}
}

func TestSystemContainsTaskPromptAndMemory(t *testing.T) {
	in := testInput()
	in.Memories = []MemoryEntry{
		{Kind: "偏好", Content: "用户喜欢简洁", Core: true},
		{Kind: "知识", Content: "仓库用 Go", Core: false},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("只有 system 时不该多出消息，得到 %d 条", len(out.Messages))
	}
	content := out.Messages[0].Content
	wants := []string{
		"当前任务：《修登录》（id=t1）",
		"你的所有动作都属于这个任务",
		MemoryPrefix,
		"（核心记忆）",
		"- [偏好] 用户喜欢简洁",
		"（相关记忆）",
		"- [知识] 仓库用 Go",
	}
	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Errorf("系统提示缺少 %q\n实际内容:\n%s", want, content)
		}
	}
	if core, rel := strings.Index(content, "（核心记忆）"), strings.Index(content, "（相关记忆）"); core < 0 || rel < 0 || core > rel {
		t.Errorf("核心记忆必须排相关记忆前面（稳定块在前）: core=%d rel=%d", core, rel)
	}
	if got := findProv(out, "system"); len(got) != 1 || got[0].Ref != "task:t1" || got[0].Index != 0 {
		t.Errorf("system provenance 不符: %+v", got)
	}
	if got := findProv(out, "memory"); len(got) != 1 || !strings.Contains(got[0].Ref, "core=1") || !strings.Contains(got[0].Ref, "relevant=1") {
		t.Errorf("memory provenance 不符: %+v", got)
	}
}

func TestTaskPromptFallbacks(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{name: "只有名字", in: Input{TaskName: "仅名字"}, want: "当前任务：《仅名字》。"},
		{name: "只有 id", in: Input{TaskID: "t9"}, want: "当前任务：《未命名任务》（id=t9）。"},
		{name: "都有", in: Input{TaskID: "t9", TaskName: "叫名字"}, want: "当前任务：《叫名字》（id=t9）。"},
		{name: "都没有", in: Input{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := mustAssemble(t, tc.in)
			if tc.want == "" {
				if out.Messages[0].Content != "" {
					t.Fatalf("没有任务时不该有任务提示，得到 %q", out.Messages[0].Content)
				}
				return
			}
			if !strings.Contains(out.Messages[0].Content, tc.want) {
				t.Fatalf("任务提示缺少 %q，得到 %q", tc.want, out.Messages[0].Content)
			}
		})
	}
}

// TestMemoryBlockShape 钉住“与 compose 一致”的形态：常量与条目行。
func TestMemoryBlockShape(t *testing.T) {
	if MemoryPrefix != "【记忆】" {
		t.Fatalf("MemoryPrefix 必须与 memory/compose.MemoryPrefix 一致，得到 %q", MemoryPrefix)
	}
	in := testInput()
	in.Memories = []MemoryEntry{{Kind: "身份", Content: "主人", Core: true}}
	out := mustAssemble(t, in)
	if !strings.Contains(out.Messages[0].Content, "- [身份] 主人") {
		t.Fatalf("记忆条目必须是 compose.EntryLine 的形态，得到 %q", out.Messages[0].Content)
	}
}

// ---------- 记忆预算 ----------

func TestMemoryCharBudgetTruncatesAndWarns(t *testing.T) {
	in := testInput()
	in.Config.CoreMaxChars = 8
	in.Memories = []MemoryEntry{
		{Kind: "a", Content: "b", Core: true},
		{Kind: "a", Content: "b", Core: true},
		{Kind: "a", Content: "b", Core: true},
	}
	out := mustAssemble(t, in)
	content := out.Messages[0].Content
	if !strings.Contains(content, TruncationMark) {
		t.Fatalf("超预算应带截断标记，得到:\n%s", content)
	}
	if !hasWarning(out, "核心记忆超出 8 字预算：截断 1 条、丢弃 1 条") {
		t.Fatalf("超预算 warning 不符: %+v", out.Warnings)
	}
}

func TestMemoryLimits(t *testing.T) {
	in := testInput()
	in.Config.CoreLimit = 1
	in.Config.RelLimit = 1
	in.Memories = []MemoryEntry{
		{Kind: "core", Content: "核心一", Core: true},
		{Kind: "core", Content: "核心二", Core: true},
		{Kind: "rel", Content: "相关一", Core: false},
		{Kind: "rel", Content: "相关二", Core: false},
	}
	out := mustAssemble(t, in)
	content := out.Messages[0].Content
	if !strings.Contains(content, "核心一") || strings.Contains(content, "核心二") {
		t.Fatalf("CoreLimit=1 应只带第一条核心记忆，得到:\n%s", content)
	}
	if !strings.Contains(content, "相关一") || strings.Contains(content, "相关二") {
		t.Fatalf("RelLimit=1 应只带第一条相关记忆，得到:\n%s", content)
	}
}

func TestMemoryNoBudgetKeeptEverything(t *testing.T) {
	in := testInput()
	in.Config.CoreMaxChars = 0
	in.Config.RelMaxChars = 0
	in.Config.CoreLimit = 0
	in.Config.RelLimit = 0
	long := strings.Repeat("长", 1000)
	in.Memories = []MemoryEntry{
		{Kind: "core", Content: long, Core: true},
		{Kind: "rel", Content: long, Core: false},
	}
	out := mustAssemble(t, in)
	if len(out.Warnings) != 0 {
		t.Fatalf("预算 <=0 表示不限制，不该截断或 warning: %+v", out.Warnings)
	}
	if strings.Count(out.Messages[0].Content, TruncationMark) != 0 {
		t.Fatal("预算 <=0 时不应出现截断标记")
	}
}

// ---------- 历史 ----------

func TestHistorySortedBySeq(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "h2", Role: "assistant", Kind: "assistant", Content: "答复", WorkID: "w1", Seq: 2},
		{ItemID: "h1", Role: "user", Kind: "user", Content: "问题", WorkID: "w1", Seq: 1},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("应得到 system + 2 条历史，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Content != "问题" || out.Messages[1].Role != "user" {
		t.Fatalf("历史应按 Seq 升序，messages[1]=%+v", out.Messages[1])
	}
	if out.Messages[2].Content != "答复" || out.Messages[2].Role != "assistant" {
		t.Fatalf("历史应按 Seq 升序，messages[2]=%+v", out.Messages[2])
	}
}

func TestHistoryWindowDropsOldestAndWarns(t *testing.T) {
	in := testInput()
	in.Config.HistoryLimit = 2
	in.History = []Record{
		{ItemID: "h3", Role: "user", Kind: "user", Content: "三", WorkID: "w1", Seq: 3},
		{ItemID: "h1", Role: "user", Kind: "user", Content: "一", WorkID: "w1", Seq: 1},
		{ItemID: "h2", Role: "user", Kind: "user", Content: "二", WorkID: "w1", Seq: 2},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("窗口 2 应只留 2 条历史，实际消息 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Content != "二" || out.Messages[2].Content != "三" {
		t.Fatalf("窗口应保留最近两条: %+v", out.Messages)
	}
	if !hasWarning(out, "历史超出窗口：丢弃最旧的 1 条（窗口 2）") {
		t.Fatalf("超窗口 warning 不符: %+v", out.Warnings)
	}
	if got := findProv(out, "window"); len(got) != 1 || got[0].Ref != "h1" || got[0].Index != -1 {
		t.Fatalf("被窗口丢弃的记录应留 provenance: %+v", got)
	}
}

func TestReasoningExcludedFromMessages(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "u1", Role: "user", Kind: "user", Content: "问题", WorkID: "w1", Seq: 1},
		{ItemID: "r1", Role: "assistant", Kind: "reasoning", Content: "思考…", WorkID: "w1", Seq: 2},
		{ItemID: "a1", Role: "assistant", Kind: "assistant", Content: "答复", WorkID: "w1", Seq: 3},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("reasoning 不该进入 messages，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	for _, m := range out.Messages {
		if strings.Contains(m.Content, "思考") {
			t.Fatalf("reasoning 内容泄漏进 messages: %+v", m)
		}
	}
	got := findProv(out, "excluded")
	if len(got) != 1 || got[0].Ref != "r1" || got[0].Index != -1 {
		t.Fatalf("reasoning 应只留 excluded provenance: %+v", got)
	}
}

func TestHistorySystemRecordExcludedWithWarning(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "s1", Role: "system", Kind: "system", Content: "旧系统提示", WorkID: "w1", Seq: 1},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("system 类历史不该进入 messages: %+v", out.Messages)
	}
	if !hasWarning(out, "是 system，已排除") {
		t.Fatalf("排除 system 记录应 warning（无静默失败）: %+v", out.Warnings)
	}
}

func TestHistoryMisplacedToolRecordExcluded(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "tc1", Role: "assistant", Kind: "tool_call", Content: "ls", WorkID: "w1", Seq: 1},
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("错放在 History 的工具记录不该渲染成普通消息: %+v", out.Messages)
	}
	if !hasWarning(out, "应放在 ToolCalls/ToolResults 里") {
		t.Fatalf("错放记录应 warning: %+v", out.Warnings)
	}
}

// ---------- 经验线与共享目录 ----------

func TestExperienceAndSharedSections(t *testing.T) {
	in := testInput()
	in.Experience = []LineHead{
		{Text: "修过网关", Count: 2, Score: 0.31, TaskID: "t-9"},
		{Text: "查过日志", Count: 5, Score: 0.82},
	}
	in.Shared = []SessionDigest{{
		Entry: "web", Title: "部署面板", Goal: "上线新面板", Outcome: "已上线",
		Capabilities: []string{"shell", "files"},
	}}
	out := mustAssemble(t, in)
	content := out.Messages[0].Content
	wants := []string{
		"（经验线头）",
		"[5 条 · 0.82] 查过日志",
		"[2 条 · 0.31] 修过网关（来自 t-9）",
		"（其他会话）",
		"- 门户「部署面板」：上线新面板；已上线（用到 shell/files）",
	}
	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Errorf("系统提示缺少 %q\n实际内容:\n%s", want, content)
		}
	}
	if strings.Index(content, "查过日志") > strings.Index(content, "修过网关") {
		t.Errorf("经验线头应按分数降序（0.82 在 0.31 前）:\n%s", content)
	}
}

// ---------- 确定性 / 前缀 / degraded ----------

func complexInput() Input {
	in := testInput()
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "帮我看看登录", WorkID: "w1", Seq: 1},
		{ItemID: "h2", Role: "assistant", Kind: "assistant", Content: "我先查一下时间", WorkID: "w1", Seq: 2},
	}
	in.ToolCalls = []Record{
		{ItemID: "tc1", Role: "assistant", Kind: "tool_call", WorkID: "w1", CallID: "call-1", Seq: 2,
			Name: "shell__run", Arguments: `{"command":"Get-Date"}`},
	}
	in.ToolResults = []Record{
		{ItemID: "tr1", Role: "tool", Kind: "tool_result", WorkID: "w1", CallID: "call-1", Seq: 3,
			Content: "2026-11-24"},
	}
	in.Inflight = []Inflight{{CallID: "call-2", Service: "shell", Action: "run", State: "running"}}
	in.ToolCalls = append(in.ToolCalls, Record{
		ItemID: "tc2", Role: "assistant", Kind: "tool_call", WorkID: "w1", CallID: "call-2", Seq: 4,
		Name: "shell__run", Arguments: `{"command":"sleep 1"}`,
	})
	in.Memories = []MemoryEntry{
		{Kind: "偏好", Content: "喜欢简洁", Core: true},
		{Kind: "知识", Content: "仓库用 Go", Core: false},
	}
	in.Experience = []LineHead{{Text: "修过网关", Count: 3, Score: 0.5, TaskID: "t-2"}}
	in.Shared = []SessionDigest{{Entry: "qq", Title: "聊天", Goal: "答疑", Outcome: "完成", Capabilities: []string{"memory"}}}
	in.Grants = []Grant{{RefKind: "message", RefID: "h1"}}
	in.Images = []ImageRef{{ID: "img-1", Gist: "报错截图", Status: "stored", First: true}}
	in.Degraded = &Degraded{Level: "warn", Reason: "images 不可用", Since: "2026-11-24T11:00:00Z", RepairHint: "检查 images 服务"}
	return in
}

func TestDeterministicAndInputUnchanged(t *testing.T) {
	in := complexInput()
	before := cloneInput(in)
	out1 := mustAssemble(t, in)
	if !reflect.DeepEqual(in, before) {
		t.Fatalf("Assemble 不能改入参（排序必须作用在副本上）")
	}
	out2 := mustAssemble(t, cloneInput(in))
	if !reflect.DeepEqual(out1, out2) {
		t.Fatalf("同 Input 两次组装必须 DeepEqual\n第一次: %+v\n第二次: %+v", out1, out2)
	}
}

func TestPrefixStableWhenHistoryTailChanges(t *testing.T) {
	in := complexInput()
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "第一问", WorkID: "w1", Seq: 1},
	}
	out1 := mustAssemble(t, in)

	in2 := cloneInput(in)
	in2.History = append(in2.History, Record{
		ItemID: "h9", Role: "user", Kind: "user", Content: "新的尾巴", WorkID: "w1", Seq: 2,
	})
	out2 := mustAssemble(t, in2)

	if out1.Messages[0].Content != out2.Messages[0].Content {
		t.Fatalf("只改历史尾部时 system 前缀必须逐字节相同\n前: %q\n后: %q",
			out1.Messages[0].Content, out2.Messages[0].Content)
	}
	if len(out2.Messages) <= len(out1.Messages) {
		t.Fatalf("新增历史后消息数应增加: %d -> %d", len(out1.Messages), len(out2.Messages))
	}
}

func TestDegradedPassthroughIsCopy(t *testing.T) {
	in := testInput()
	in.Degraded = &Degraded{Level: "warn", Reason: "shared 不可用", Since: "s", RepairHint: "r"}
	out := mustAssemble(t, in)
	if out.Degraded == nil || *out.Degraded != *in.Degraded {
		t.Fatalf("degraded 应原样透传: %+v", out.Degraded)
	}
	in.Degraded.Level = "fatal"
	if out.Degraded.Level != "warn" {
		t.Fatalf("Output.Degraded 应是拷贝，调用方后续修改不应影响它: %+v", out.Degraded)
	}
}

// ---------- 零值 Config 的约定 ----------

func TestZeroConfigMeansNoBudgetsNotSilentDrop(t *testing.T) {
	in := testInput()
	in.Config = Config{} // 全部零值：不设窗口、不限制、不截断、不带图
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "一", WorkID: "w1", Seq: 1},
		{ItemID: "h2", Role: "user", Kind: "user", Content: "二", WorkID: "w1", Seq: 2},
		{ItemID: "h3", Role: "user", Kind: "user", Content: "三", WorkID: "w1", Seq: 3},
	}
	in.Memories = []MemoryEntry{
		{Kind: "core", Content: strings.Repeat("长", 500), Core: true},
		{Kind: "rel", Content: "相关", Core: false},
	}
	in.Images = []ImageRef{{ID: "img-1", Gist: "猫", First: true}}

	out := mustAssemble(t, in)
	if len(out.Warnings) != 0 {
		t.Fatalf("零值预算不该产生 warning: %+v", out.Warnings)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("零值 Config 应带全部 3 条历史且不带图，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	content := out.Messages[0].Content
	if !strings.Contains(content, "（核心记忆）") || !strings.Contains(content, "（相关记忆）") {
		t.Fatalf("零值 Config 不该丢掉记忆: %s", content)
	}
	if strings.Contains(content, TruncationMark) {
		t.Fatal("零值 Config 表示不截断")
	}
	for _, m := range out.Messages[1:] {
		if strings.Contains(m.Content, "图片") {
			t.Fatalf("IncludeImages=false 时不该渲染图片行: %+v", m)
		}
	}
}
