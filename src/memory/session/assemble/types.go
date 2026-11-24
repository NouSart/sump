// Package assemble 是 memory 的唯一组装者（S-001 不变量 INV-1 / INV-3）：
// 输入一份不可变快照（全部来自 memory 自己的库 + 配置），输出一份可直接交给 LLM
// 的 messages。所有“看什么”的决策（历史窗口、记忆块、任务提示、工具配对与截断、
// 图片索引、可见性）都在这里，调用方不再二次拼装。
//
// 纯函数边界（R-011 §2）：
//   - 零 I/O：不读库、不读文件、不访问网络、不取进程环境；
//   - 不读 time.Now：当前渲染没有“相对时间”字段，in.Now 原样保留给将来的时间渲染；
//   - 同一 Input 必须产出同一 Output（凡是排序都显式排序且稳定，map 只做查表不参与遍历输出）；
//   - 依赖缺失不返回 error：降级只写 warnings / 原样透传 degraded，绝不因“缺一块”失败。
//
// 与 facade/compose 的对应：记忆块仍以 compose.MemoryPrefix（【记忆】）开头，条目仍是
// “- [kind] content”。本包不 import compose（组装是独立纯函数），常量与形态在这里
// 各写一份，由测试钉住两者一致（见 TestMemoryBlockShape）。
package assemble

import "time"

const (
	// MemoryPrefix 是记忆块的内容前缀，与 memory/compose.MemoryPrefix 同字面量：
	// 调用方靠它识别并替换自己的旧注入，两边必须一致。
	MemoryPrefix = "【记忆】"

	// TruncationMark 是截断标记：工具输出超预算、记忆条目超预算时都追加它，
	// 让模型知道“后面还有，只是没带进来”。
	TruncationMark = "…[truncated]"
)

// 默认预算（与 experience 的默认设置对齐；见 experience/experience.go 的 default* 常量）。
// 调用方一般从 DefaultConfig() 出发再按设置覆盖。
const (
	DefaultHistoryLimit       = 12
	DefaultCoreLimit          = 3
	DefaultCoreMaxChars       = 400
	DefaultRelLimit           = 5
	DefaultRelMaxChars        = 800
	DefaultMaxToolOutputChars = 8000
)

// ToolCall 是一次工具调用在模型输入里的形态（对应 OpenAI 的 tool_calls 元素）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Message 是一条可直接交给 LLM 的消息。
// 与 protocol.Message 同形，但不带线格式标签：组装只产出结构，序列化由 server 层做。
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// Record 是快照里的一条会话记录（messages 表一行的投影）。
//
// 说明：Name/Arguments 是工具调用记录（kind=tool_call）的必要载荷——模型输入里的
// assistant.tool_calls 必须带工具名与参数；Content 是**给人看的轨迹原文**（例如 shell
// 命令），不能当参数回灌。其余 kind 的记录这两个字段为空。
type Record struct {
	ItemID     string
	Role       string
	Kind       string // user / assistant / reasoning / tool_call / tool_result / spawn_return
	Content    string
	WorkID     string
	Visibility string // work / thread / task；空 = work（默认不可见）
	CallID     string // 工具调用 id（kind=tool_call / tool_result 时）
	Seq        int64  // 会话内单调序号（排序唯一依据）

	Name      string // 仅 kind=tool_call：工具名（如 shell__run）
	Arguments string // 仅 kind=tool_call：JSON 参数原文
}

// Inflight 是一次已发出、尚未拿到结果的调用（memory 的 tool_inflight 投影）。
type Inflight struct {
	CallID  string
	Service string
	Action  string
	State   string // running / awaiting；只用于人读，占位文本不区分
}

// MemoryEntry 是一条长期记忆。
type MemoryEntry struct {
	Kind    string
	Content string
	// Core 区分核心记忆（稳定块：每次对话都带）与相关记忆（易变块：按查询召回）。
	// 为什么需要它：Input 只有一份 Memories，必须知道哪几条进稳定前缀，否则
	// CoreMaxChars/RelMaxChars 两个预算无从分配。
	Core bool
}

// LineHead 是经验网的一条“线头”（同簇代表句 + 条数 + 分数 + 来自哪个任务）。
type LineHead struct {
	Text   string
	Count  int
	Score  float64
	TaskID string
}

// SessionDigest 是共享库里别的会话的摘要（只有摘要，没有原文）。
type SessionDigest struct {
	Entry        string
	Title        string
	Goal         string
	Outcome      string
	Capabilities []string
}

// Grant 是一条跨 work 的显式上下文授权（work_context_grants 一行的投影）。
type Grant struct {
	RefKind string // message / snapshot / memory
	RefID   string
}

// ImageRef 是图片索引的一行。纯函数不取字节，只渲染 gist 文本。
type ImageRef struct {
	ID     string
	Gist   string
	Status string // stored / lost
	First  bool   // 本轮第一次看到（首次才带图片 id 与前缀，之后只留 gist 行）
}

// Degraded 是降级状态（fatal / severe / warn），原样透传，不在组装里解释。
type Degraded struct {
	Level      string
	Reason     string
	Since      string
	RepairHint string
}

// Trigger 是“这一轮为什么醒”：user / tool_result / timer / resume / spawn。
// 当前装配顺序固定、不按 trigger 分支；保留它给 provenance 与调用方对账。
type Trigger struct {
	Kind string
	Item string
}

// Config 是组装预算。
//
// **字段按字面使用（零值 = 关闭对应预算），不做“零值兜底”**——避免调用方少填一个
// 字段就静默改变模型看到的东西。调用方一般先 DefaultConfig() 再按设置覆盖：
//   - HistoryLimit > 0 才做历史窗口；<= 0 表示整段历史都带；
//   - CoreLimit / RelLimit > 0 才限制条数；<= 0 表示不限条数；
//   - CoreMaxChars / RelMaxChars > 0 才做字符预算；<= 0 表示不截断；
//   - MaxToolOutputChars > 0 才截断工具输出；<= 0 表示原样返回；
//   - IncludeImages 为 true 才把图片索引渲染成文本行（false = 本轮不带图）。
type Config struct {
	HistoryLimit       int
	CoreLimit          int
	CoreMaxChars       int
	RelLimit           int
	RelMaxChars        int
	MaxToolOutputChars int
	IncludeImages      bool
}

// Input 是一次组装的完整输入（不可变快照）。切片顺序不参与决策，除非另有说明：
// History/ToolCalls/ToolResults 会按 Seq 稳定排序，Experience 按 Score 稳定排序，
// 其余保持调用方给的顺序。
type Input struct {
	WorkID         string
	TaskID         string
	ConversationID string
	Owner          string
	Entry          string
	TaskName       string

	History     []Record // messages：窗口内 user/assistant（数据异常时也容错）
	ToolCalls   []Record // kind=tool_call
	ToolResults []Record // kind=tool_result，按 call_id 配对
	Inflight    []Inflight

	Memories   []MemoryEntry
	Experience []LineHead
	Shared     []SessionDigest
	Grants     []Grant
	Images     []ImageRef

	Config   Config
	Trigger  Trigger
	Degraded *Degraded
	Now      time.Time
}

// Provenance 说明“这条消息/记录从哪来”，供 explain_context 回答“为什么带上了它”。
// Index 是 Output.Messages 的下标；-1 表示没有进入 messages（filtered / excluded /
// window / orphan / duplicate），只留证据。
type Provenance struct {
	Index  int
	Source string
	Ref    string
}

// Output 是组装结果。
// Degraded 是 Input.Degraded 的深拷贝（调用方之后改自己的结构不会影响这份结果）。
type Output struct {
	Messages   []Message
	Provenance []Provenance
	Warnings   []string
	Degraded   *Degraded
}

// DefaultConfig 返回生产默认预算（与 memory/server 的默认设置一致）。
func DefaultConfig() Config {
	return Config{
		HistoryLimit:       DefaultHistoryLimit,
		CoreLimit:          DefaultCoreLimit,
		CoreMaxChars:       DefaultCoreMaxChars,
		RelLimit:           DefaultRelLimit,
		RelMaxChars:        DefaultRelMaxChars,
		MaxToolOutputChars: DefaultMaxToolOutputChars,
		IncludeImages:      true,
	}
}
