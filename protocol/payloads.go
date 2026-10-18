package protocol

import (
	"math"

	"github.com/vmihailenco/msgpack/v5"
)

// Provide 描述服务对外提供的一个动作（“接受什么输入、有什么输出”）。
type Provide struct {
	Action string `msgpack:"action"`
	Input  string `msgpack:"input,omitempty"`
	Output string `msgpack:"output,omitempty"`
	// Tool 显式声明该动作是否作为 LLM 工具暴露给模型（默认 false）。
	// 工具名 = <service>__<action>；链机制保留动作与调试动作即便声明也不暴露（第二道闸）。
	Tool bool `msgpack:"tool,omitempty"`
	// Async 显式声明该动作是“受理即回执”：响应只表示已受理（带回句柄 item_id），
	// 后续进展由该服务以 report_result 补报送达（可能晚于调用方收尾）。
	Async bool `msgpack:"async,omitempty"`
}

// Setting 是服务的一个可设置项。
// 三种声明（互斥）：
//   - 普通项：值由设置中心保管（覆盖值落盘于中心），注册时全量对齐下发；
//   - Secret（`Secret`）：值**不进设置中心**（不落盘、不回显、list 恒回空串）——
//     中心只转发一次，由**用它服务自己**持久化（见 service 骨架的 HandleSecret）；
//     `Set` 表示该服务当前是否已有值（面板据此显示「已设置」，值本身永不上线）；
//   - ReadOnly（`ReadOnly`）：启动参数带起来的只读值，进面板可见但不可改（`Default` 即启动值）。
type Setting struct {
	Key      string `msgpack:"key"`
	Default  string `msgpack:"default,omitempty"`
	Secret   bool   `msgpack:"secret,omitempty"`
	ReadOnly bool   `msgpack:"readonly,omitempty"`
	Set      bool   `msgpack:"set,omitempty"` // 仅 Secret 有意义：服务侧当前是否已有值
	// 以下是给**人**看的部分（可选；缺省时面板退回显示 Key）。
	Label   string   `msgpack:"label,omitempty"`   // 中文名，如「单文件大小上限」
	Hint    string   `msgpack:"hint,omitempty"`    // 一句话：改它会发生什么
	Kind    string   `msgpack:"kind,omitempty"`    // bool | number | enum | textarea | text | service | optional-service（缺省 text）
	Options []string `msgpack:"options,omitempty"` // Kind=enum 的候选值
	Unit    string   `msgpack:"unit,omitempty"`    // Kind=number 的单位符号（ms/MB/KB/chars/count），面板翻成人话
}

// RegisterPayload 是 register 请求的 payload：服务名片。
type RegisterPayload struct {
	Name        string    `msgpack:"name"`
	Addr        string    `msgpack:"addr"`
	Title       string    `msgpack:"title,omitempty"` // 中文短名（面板导航用），如「审批」
	Description string    `msgpack:"description,omitempty"`
	Provides    []Provide `msgpack:"provides,omitempty"`
	Settings    []Setting `msgpack:"settings,omitempty"`
	HeartbeatMS int64     `msgpack:"heartbeat_ms,omitempty"` // 心跳间隔（毫秒；0 = 未声明，中心用默认）
}

// ServiceCard 是 roster 中一个服务的公开部分。
type ServiceCard struct {
	Name        string    `msgpack:"name"`
	Addr        string    `msgpack:"addr"`
	Title       string    `msgpack:"title,omitempty"` // 中文短名（面板导航用）
	Description string    `msgpack:"description,omitempty"`
	Provides    []Provide `msgpack:"provides,omitempty"`
	HeartbeatMS int64     `msgpack:"heartbeat_ms,omitempty"` // 心跳间隔（毫秒；离线判定用）
}

// Card 返回名片去掉私有部分（settings）后的公开视图。
func (r RegisterPayload) Card() ServiceCard {
	return ServiceCard{
		Name:        r.Name,
		Addr:        r.Addr,
		Title:       r.Title,
		Description: r.Description,
		Provides:    r.Provides,
		HeartbeatMS: r.HeartbeatMS,
	}
}

// ResponsePayload 是通用响应的 payload。
type ResponsePayload struct {
	OK    bool               `msgpack:"ok"`
	Data  msgpack.RawMessage `msgpack:"data,omitempty"`
	Error string             `msgpack:"error,omitempty"`
}

// RosterPayload 是全局服务清单（register 响应与 roster 事件共用）。
type RosterPayload struct {
	Services []ServiceCard `msgpack:"services"`
	Revision int64         `msgpack:"revision"`
}

// JumpPayload 是一次跳转访问：目标服务上的动作 + 输入。
type JumpPayload struct {
	Action string             `msgpack:"action"`
	Input  msgpack.RawMessage `msgpack:"input,omitempty"`
}

// HeartbeatPayload 是报活 payload。
type HeartbeatPayload struct {
	Status string `msgpack:"status,omitempty"`
}

// ---------- 会话（一段能起名字、能归档的上下文） ----------

// Conversation 是一个会话的元信息：历史记录页按它分行列出。
// 消息与任务是它的**内容**，这里只存"这一行怎么被列出来"——不给消息数、任务数做冗余，
// 派生数据一律现算（否则两处真相迟早对不上）。
// Title 为空 = 还没起名（界面自己回退一个占位，别在库里造假名字）。
// Entry 是入口服务名（门户 / qq / shell）：同一套记忆，进来的门不同。
// Status：active = 可在用；archived = 收起了（只读，列表默认不显示）。
type Conversation struct {
	ID        string `msgpack:"id"`
	Title     string `msgpack:"title,omitempty"`
	Entry     string `msgpack:"entry,omitempty"`
	Status    string `msgpack:"status"`
	CreatedAt string `msgpack:"created_at"`
	UpdatedAt string `msgpack:"updated_at"`
}

// ConversationListPayload 列出会话（按最近活跃排序）。
type ConversationListPayload struct {
	Limit int `msgpack:"limit,omitempty"`
	// IncludeArchived 为真时连归档的一起列（默认只列 active）。
	IncludeArchived bool `msgpack:"include_archived,omitempty"`
}

// ConversationListResult 是会话清单。
type ConversationListResult struct {
	Conversations []Conversation `msgpack:"conversations"`
}

// ConversationRenamePayload 改会话名（空标题 = 清掉名字，回到未命名）。
type ConversationRenamePayload struct {
	ID    string `msgpack:"id"`
	Title string `msgpack:"title"`
}

// ConversationResult 是单个会话查询/变更的结果。
type ConversationResult struct {
	Conversation Conversation `msgpack:"conversation"`
}

// ---------- 任务（一段有名字、有始有终的工作） ----------

// Task 是一个任务：每条会话记录都归属某个任务。
// Status：active = 进行中（保留全部上下文，可恢复）；done = 已完结（只读，不可继续）。
// 任务**只存事实**：谁在什么时候叫什么名字、什么状态。完成判定由 reasoner 负责，
// 记忆服务不解释任务语义（只给出"当前是哪个任务"）。
type Task struct {
	ID             string `msgpack:"id"`
	ConversationID string `msgpack:"conversation_id"`
	Name           string `msgpack:"name"`
	Status         string `msgpack:"status"`
	CreatedAt      string `msgpack:"created_at"`
	UpdatedAt      string `msgpack:"updated_at"`
}

// TaskCurrentPayload 取当前任务（最近活跃的 active 任务；没有就是空）。
type TaskCurrentPayload struct {
	ConversationID string `msgpack:"conversation_id"`
}

// TaskOpenPayload 新开一个任务（调用方保证此刻没有进行中的任务）。
type TaskOpenPayload struct {
	ConversationID string `msgpack:"conversation_id"`
	Name           string `msgpack:"name"`
}

// TaskFinishPayload 完结一个任务（可顺带把名字改准）。
type TaskFinishPayload struct {
	TaskID string `msgpack:"task_id"`
	Name   string `msgpack:"name,omitempty"`
}

// TaskRenamePayload 改任务名（进行中的任务也可以改：模型判定时顺手把标题写准）。
type TaskRenamePayload struct {
	TaskID string `msgpack:"task_id"`
	Name   string `msgpack:"name"`
}

// TaskResumePayload 恢复一个未完结任务（把它顶成当前任务）。
type TaskResumePayload struct {
	TaskID string `msgpack:"task_id"`
}

// TaskListPayload 列出某会话的任务（按最近活跃排序）。
type TaskListPayload struct {
	ConversationID string `msgpack:"conversation_id"`
	Limit          int    `msgpack:"limit,omitempty"`
}

// TaskResult 是单个任务查询/变更的结果（Task 为 nil = 没有当前任务）。
type TaskResult struct {
	Task *Task `msgpack:"task,omitempty"`
}

// TaskListResult 是任务清单。
type TaskListResult struct {
	Tasks []Task `msgpack:"tasks"`
}

// TaskResumeResult 是恢复任务的结果：任务本身 + 该任务的记录。
type TaskResumeResult struct {
	Task     Task           `msgpack:"task"`
	Messages []HistoryEntry `msgpack:"messages"`
}

// ---------- 记忆往返（完全启动链） ----------

// TaskContext 是 LLM 服务的任务上下文：随“记忆往返”托管给记忆服务。
// Messages 是推理中的完整消息链；Boss 是原任务发起者（不可丢失，由记忆服务原样带回）。
// Task 是这次工作所属的任务：由记忆服务在 recall 时确定（没有进行中的任务就地新建），
// 随 resume 带回 reasoner——**没有任务这条链就走不起来**。
type TaskContext struct {
	ConversationID string    `msgpack:"conversation_id,omitempty"`
	Messages       []Message `msgpack:"messages"`
	Boss           string    `msgpack:"boss,omitempty"`
	Task           *Task     `msgpack:"task,omitempty"`
}

// RecallPayload 是 recall（向记忆请求记忆）的 payload：把任务上下文交给记忆服务。
type RecallPayload struct {
	Context TaskContext `msgpack:"context"`
}

// ResumePayload 是 resume 的 payload：记忆服务组装好上下文后发回，
// 调用方收到来自记忆的消息时才“完全启动”（调用 LLM API）。
type ResumePayload struct {
	Context TaskContext `msgpack:"context"`
}

// StorePayload 是 store 的 payload：向记忆服务记一条会话记录。
// Kind 区分记录类型（空 = 按 Role 推断）：user / assistant / reasoning / tool_call / tool_result。
// Seq 是**会话内单调序号**（由写入方给）：让并发写（Fire 是 goroutine）也能按真实先后落库与排序。
// Meta 是给人看的附加信息（JSON 字符串，如工具名、命令、call_id、是否截断）。
// TaskID 是这条记录属于哪个任务（空 = 由记忆服务归到当前任务）。
// Boss 是这条记录所属会话的入口服务（门户 / qq）：记忆服务只在会话第一次落记录时记下它，
// 用来在历史记录里分清"这段对话是从哪扇门进来的"。空值不改已有入口。
type StorePayload struct {
	ConversationID string `msgpack:"conversation_id"`
	Boss           string `msgpack:"boss,omitempty"`
	Role           string `msgpack:"role"`
	Content        string `msgpack:"content"`
	Kind           string `msgpack:"kind,omitempty"`
	Meta           string `msgpack:"meta,omitempty"`
	Seq            int64  `msgpack:"seq,omitempty"`
	TaskID         string `msgpack:"task_id,omitempty"`
}

// HistoryPayload 是 history 的 payload：向记忆服务要某会话最近的消息（供门户回填对话视图）。
// AfterSeq > 0 时只取序号更大的记录（增量拉取，避免每次全量重拉）。
// TaskID 非空时只取该任务的记录；为空时取**当前任务**（门户默认看到的那段）。
// AllTasks 为真时取整个会话（门户按任务分段展示时要的就是这一段）。
type HistoryPayload struct {
	ConversationID string `msgpack:"conversation_id"`
	Limit          int    `msgpack:"limit,omitempty"`
	AfterSeq       int64  `msgpack:"after_seq,omitempty"`
	TaskID         string `msgpack:"task_id,omitempty"`
	AllTasks       bool   `msgpack:"all_tasks,omitempty"`
}

// HistoryEntry 是一条落库的会话记录（带落库时间，供界面按时间排序）。
type HistoryEntry struct {
	ID      int64  `msgpack:"id"`
	Seq     int64  `msgpack:"seq,omitempty"`
	Role    string `msgpack:"role"`
	Kind    string `msgpack:"kind,omitempty"`
	Content string `msgpack:"content"`
	Meta    string `msgpack:"meta,omitempty"`
	At      string `msgpack:"at"`
	TaskID  string `msgpack:"task_id,omitempty"`
}

// HistoryResult 是 history 的结果：按时间正序。
type HistoryResult struct {
	ConversationID string         `msgpack:"conversation_id"`
	Messages       []HistoryEntry `msgpack:"messages"`
}

// ProgressPayload 是 progress 的 payload：任务进行中的一条实况（思考 / 命令 / 输出），
// 推给**声明了 provides=progress** 的入口方（门户）。收到即渲染，不必等最终答复。
// 与 store 同源同 seq：落库是真相，progress 只是让界面不必等到最后。
type ProgressPayload struct {
	ConversationID string `msgpack:"conversation_id"`
	TaskID         string `msgpack:"task_id,omitempty"`
	Seq            int64  `msgpack:"seq"`
	Kind           string `msgpack:"kind"` // reasoning / tool_call / tool_result
	Content        string `msgpack:"content"`
	Meta           string `msgpack:"meta,omitempty"`
	At             string `msgpack:"at,omitempty"`
}

// ---------- 任务链入口 ----------

// UserMessagePayload 是 user_message（链入口）的 payload：用户消息文本 + 图片/文件引用。
type UserMessagePayload struct {
	Text           string   `msgpack:"text"`
	ConversationID string   `msgpack:"conversation_id,omitempty"`
	Images         []string `msgpack:"images,omitempty"` // 图片引用（images 服务中的 id）
	Files          []string `msgpack:"files,omitempty"`  // 文件引用（files 服务中的 id；占位文本亦随行，供模型凭 id 调 files__read）
}

// ---------- 交付与图片 ----------

// DeliverPayload 是 deliver（链终点 → boss）的 payload：最终文本 + 会话标识（boss 据此路由回复）。
type DeliverPayload struct {
	Text           string `msgpack:"text"`
	ConversationID string `msgpack:"conversation_id,omitempty"`
	TaskID         string `msgpack:"task_id,omitempty"` // 这条答复属于哪个任务（门户据此刻画）
}

// ImageSavePayload 是 images.save 的 payload：URL 与 base64 二选一。
type ImageSavePayload struct {
	URL  string `msgpack:"url,omitempty"`
	Data string `msgpack:"data,omitempty"` // base64（不含 data: 前缀）
	Mime string `msgpack:"mime,omitempty"` // 可选，缺省时探测
}

// ImageSaveResult 是 images.save 的结果。
type ImageSaveResult struct {
	ID   string `msgpack:"id"`
	Mime string `msgpack:"mime"`
	Size int64  `msgpack:"size"`
}

// ImageFetchPayload 是 images.fetch 的 payload。
type ImageFetchPayload struct {
	ID string `msgpack:"id"`
}

// ImageFetchResult 是 images.fetch 的结果（存入时二进制，取时转 base64）。
type ImageFetchResult struct {
	ID   string `msgpack:"id"`
	Mime string `msgpack:"mime"`
	Data string `msgpack:"data"` // base64（不含 data: 前缀）
	Size int64  `msgpack:"size"`
}

// ---------- 文件链（files 服务） ----------

// FileSavePayload 是 files.save 的 payload：URL 与 base64 二选一；filename 必填
// （占位展示与文本类判断用——文件与图片的结构差异就在名字）。
type FileSavePayload struct {
	URL      string `msgpack:"url,omitempty"`
	Data     string `msgpack:"data,omitempty"` // base64（不含 data: 前缀）
	Filename string `msgpack:"filename"`
	Mime     string `msgpack:"mime,omitempty"` // 可选，缺省时探测
}

// FileSaveResult 是 files.save 的结果。
type FileSaveResult struct {
	ID       string `msgpack:"id"`
	Filename string `msgpack:"filename"`
	Mime     string `msgpack:"mime"`
	Size     int64  `msgpack:"size"`
}

// FileFetchPayload 是 files.fetch 的 payload。
type FileFetchPayload struct {
	ID string `msgpack:"id"`
}

// FileFetchResult 是 files.fetch 的结果（存入时二进制，取时转 base64）。
type FileFetchResult struct {
	ID       string `msgpack:"id"`
	Filename string `msgpack:"filename"`
	Mime     string `msgpack:"mime"`
	Data     string `msgpack:"data"` // base64（不含 data: 前缀）
	Size     int64  `msgpack:"size"`
}

// FileReadPayload 是 files.read 的 payload：读取文件的文本内容（LLM 工具，凭 id 自取）。
type FileReadPayload struct {
	ID string `msgpack:"id"`
}

// FileReadResult 是 files.read 的结果：文本类文件的内容（超长按 read.max_chars 截断）；
// 二进制文件不支持阅读——以错误形式回绝，不返回本结构。
type FileReadResult struct {
	ID        string `msgpack:"id"`
	Filename  string `msgpack:"filename"`
	Text      string `msgpack:"text"`
	Truncated bool   `msgpack:"truncated"` // true = 内容被截断，未给全（模型可据此决定后续动作）
}

// FileWriteEdit 是一次单元格写入：A1 风格坐标 + 值（v0 统一字符串）。
type FileWriteEdit struct {
	Cell  string `msgpack:"cell"`
	Value string `msgpack:"value"`
}

// FileWritePayload 是 files.write 的 payload：编辑 xlsx 的单元格（就地修改权威副本）。
type FileWritePayload struct {
	ID    string          `msgpack:"id"`
	Sheet string          `msgpack:"sheet"`
	Edits []FileWriteEdit `msgpack:"edits"`
}

// FileWriteResult 是 files.write 的结果：实际写入的单元格数。
type FileWriteResult struct {
	ID      string `msgpack:"id"`
	Sheet   string `msgpack:"sheet"`
	Updated int    `msgpack:"updated"`
}

// WordEditOperation 是 files.edit_word 的一次操作。
// v0 仅支持 op="replace"；后续可通过新增 op 类型扩展复杂编辑能力。
type WordEditOperation struct {
	Op  string `msgpack:"op"`            // 操作类型：replace
	Old string `msgpack:"old,omitempty"` // replace 时被替换的文本
	New string `msgpack:"new,omitempty"` // replace 时的新文本
}

// FileWordEditPayload 是 files.edit_word 的 payload：就地修改 .docx 正文。
type FileWordEditPayload struct {
	ID         string              `msgpack:"id"`
	Operations []WordEditOperation `msgpack:"operations"`
}

// FileWordEditResult 是 files.edit_word 的结果：实际替换的条数。
type FileWordEditResult struct {
	ID       string `msgpack:"id"`
	Replaced int    `msgpack:"replaced"`
}

// ---------- 设置中心：设置存取 ----------

// SettingView 是一个设置项的当前状态（设置中心对外呈现）。
// Secret 项的 Value 恒为空串（中心根本没有值可回：秘密由服务自己持有）；
// ReadOnly 项的 Value 是服务的启动值，不可改。
type SettingView struct {
	Key        string `msgpack:"key"`
	Value      string `msgpack:"value"`              // 当前生效值（中心口径：覆盖值 ?? 声明预设；Secret 恒空；ReadOnly 为启动值）
	Default    string `msgpack:"default"`            // 服务注册时声明的默认值（Secret 恒空）
	Overridden bool   `msgpack:"overridden"`         // 是否处于覆盖状态（Secret 恒 false）
	Secret     bool   `msgpack:"secret,omitempty"`   // 值由服务自己持有，不回显
	ReadOnly   bool   `msgpack:"readonly,omitempty"` // 不可改（需重启改启动参数）
	Set        bool   `msgpack:"set,omitempty"`      // 仅 Secret：服务侧当前是否已有值
	Actual     string `msgpack:"actual"`             // 服务侧自报的实际值（没回传过为空串）
	Reported   bool   `msgpack:"reported,omitempty"` // 服务是否回传过实际值（Actual 为空串时靠它区分"没报过"与"报了空值"）
	// 给**人**看的部分：由服务声明时给出，面板据此画中文名、说明与合适的控件。
	Label   string   `msgpack:"label,omitempty"`
	Hint    string   `msgpack:"hint,omitempty"`
	Kind    string   `msgpack:"kind,omitempty"`
	Options []string `msgpack:"options,omitempty"`
	Unit    string   `msgpack:"unit,omitempty"`
}

// ServiceSettings 是一个服务的设置清单（按声明顺序）。
type ServiceSettings struct {
	Service  string        `msgpack:"service"`
	Settings []SettingView `msgpack:"settings,omitempty"`
}

// SettingsListPayload 是 list_settings 的输入：可选按服务过滤（服务名）。
type SettingsListPayload struct {
	Service string `msgpack:"service,omitempty"`
}

// SettingsResult 是设置查询 / 变更的输出（list_settings / set_setting / reset_setting）：
// list 返回全部（或过滤后的）服务；set / reset 返回被改动的那个服务。
type SettingsResult struct {
	Services []ServiceSettings `msgpack:"services"`
}

// SetSettingPayload 是 set_setting 的输入：把某服务的某项设为某值（写覆盖值）。
type SetSettingPayload struct {
	Service string `msgpack:"service"`
	Key     string `msgpack:"key"`
	Value   string `msgpack:"value"`
}

// ResetSettingPayload 是 reset_setting 的输入：清除覆盖值，回落服务声明的默认值。
type ResetSettingPayload struct {
	Service string `msgpack:"service"`
	Key     string `msgpack:"key"`
}

// ConfigurePayload 是 configure 的输入：设置中心把生效值下发给所属服务
// （注册成功后全量逐键下发；set_setting / reset_setting 后单键下发）。
type ConfigurePayload struct {
	Key        string `msgpack:"key"`
	Value      string `msgpack:"value"`      // 生效值 = 覆盖值 ?? 声明默认值
	Default    string `msgpack:"default"`    // 服务声明的默认值
	Overridden bool   `msgpack:"overridden"` // 是否处于覆盖状态
}

// ReportSettingPayload 是「服务 → 设置中心」的设置回传（动作 report_setting）：
// 服务侧的值**自己变了**时主动报个事实，中心据此更新服务侧实际值（面板实时可见），
// 它**不改**中心手里的覆盖值（谁的值是谁的，两边都看得见）。
// Secret 项 Value 恒空（值永不上线：只回「有没有值」）；Seq 服务内单调递增，
// 中心丢弃乱序到达的旧报（补报与回传的到达顺序都没有保证）。
type ReportSettingPayload struct {
	Service string `msgpack:"service"`
	Key     string `msgpack:"key"`
	Value   string `msgpack:"value,omitempty"`
	Set     bool   `msgpack:"set,omitempty"`
	Seq     int64  `msgpack:"seq,omitempty"`
}

// NormalizeJSONNumbers 把 JSON 解出来的数字归一化：整数值的 float64 → int64，其余原样（递归）。
//
// 为什么需要它：LLM 给的工具参数是 JSON，`encoding/json` 把数字一律解成 float64，
// 直接 msgpack 编码会写成浮点（0xcb），而服务侧载荷里的整数字段（ms、priority…）解码会
// 直接失败（"msgpack: invalid code=cb decoding int64"）。凡是把 **LLM 给的 JSON 转成协议载荷**
// 的地方，都先过一遍这个函数。
func NormalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && t >= -9223372036854775808 && t < 9223372036854775808 {
			return int64(t)
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = NormalizeJSONNumbers(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = NormalizeJSONNumbers(val)
		}
		return t
	default:
		return v
	}
}

// SettingResult 是一次下发中单个设置项的结果（settings 事件用）。
type SettingResult struct {
	Key   string `msgpack:"key"`
	Value string `msgpack:"value,omitempty"` // 成功应用后的生效值
	OK    bool   `msgpack:"ok"`
	Error string `msgpack:"error,omitempty"`
}

// SettingsEventPayload 是 settings 事件的载荷：中心广播某服务的设置变更与下发结果。
// Cause 复用触发的动作名（register / set_setting / reset_setting）。
type SettingsEventPayload struct {
	Cause   string          `msgpack:"cause"`
	Service string          `msgpack:"service"`
	Results []SettingResult `msgpack:"results,omitempty"`
}

// ---------- embedding（文本 → 向量） ----------

// EmbedPayload 是 embed 的输入：一批文本；Query 标记查询侧（服务按模型建议决定是否加指令前缀）。
type EmbedPayload struct {
	Texts []string `msgpack:"texts"`
	Query bool     `msgpack:"query,omitempty"`
}

// EmbedResult 是 embed 的输出：与 texts 一一对应的向量（L2 归一化）+ 维度 + 模型标识
// （模型标识用于判定已存向量是否失效）。向量在线路上是 float64（Python/JS 原生数字），
// Go 消费侧按需转 float32。
type EmbedResult struct {
	Vectors [][]float64 `msgpack:"vectors"`
	Dim     int         `msgpack:"dim"`
	Model   string      `msgpack:"model"`
}

// ---------- 异步受理的补报（report_result） ----------

// AsyncResultPayload 是 report_result 的载荷：一个“受理即回执”（Provide.Async）的动作
// 在受理之后发回的后续消息（补报）。方向与服务 → 调用方（信封 to = 原先的调用方）。
// ItemID 是受理回执里给出的句柄（调用方据此把补报配回自己的记账）；
// Phase 是进展阶段（服务自定，如 approved / rejected / done / failed）；
// Text 是可直接阅读的文本（调用方注入模型上下文 / 呈现给用户）；Data 是服务自定的结构化补充；
// Final 标记这是该次受理的最后一条（调用方收到后释放记账）。
type AsyncResultPayload struct {
	ItemID string             `msgpack:"item_id"`
	Phase  string             `msgpack:"phase"`
	Text   string             `msgpack:"text,omitempty"`
	Data   msgpack.RawMessage `msgpack:"data,omitempty"`
	Final  bool               `msgpack:"final,omitempty"`
}

// ---------- shell（命令执行服务） ----------

// ShellRunPayload 是 shell.run 的输入：一条待执行的命令。
type ShellRunPayload struct {
	Command string `msgpack:"command"`            // 命令行（交给服务选定的 shell 解释）
	WorkDir string `msgpack:"work_dir,omitempty"` // 工作目录（默认服务根目录；越界即拒）
}

// ShellRunResult 是 shell.run 的受理回执：命令已入队列，执行结果由 report_result 补报送达。
// Phase 与补报（AsyncResultPayload.Phase）用同一词汇：受理回执就是这条命令的第一个阶段（queued）。
type ShellRunResult struct {
	ItemID string `msgpack:"item_id"` // 受理句柄（补报原样带回）
	Phase  string `msgpack:"phase"`   // 阶段：queued
}

// ShellEventData 是 shell 补报的结构化补充（report_result 的 data）。
type ShellEventData struct {
	Approval    *CommandApprovalResult `msgpack:"approval,omitempty"` // 第一层裁决及完整 LLM 反馈
	Command     string                 `msgpack:"command,omitempty"`
	WorkDir     string                 `msgpack:"work_dir,omitempty"`
	Shell       string                 `msgpack:"shell,omitempty"` // 实际使用的 shell 可执行文件
	ExitCode    int                    `msgpack:"exit_code"`       // 退出码（未执行 / 超时终止时为 0 / -1）
	Stdout      string                 `msgpack:"stdout,omitempty"`
	Stderr      string                 `msgpack:"stderr,omitempty"`
	StdoutBytes int64                  `msgpack:"stdout_bytes,omitempty"` // 实际收到多少字节（截断前的总数）
	StderrBytes int64                  `msgpack:"stderr_bytes,omitempty"`
	TimedOut    bool                   `msgpack:"timed_out,omitempty"`
	Truncated   bool                   `msgpack:"truncated,omitempty"` // 输出超过上限被截断
	DurationMS  int64                  `msgpack:"duration_ms,omitempty"`
	Error       string                 `msgpack:"error,omitempty"` // 未执行 / 未能执行完的原因
}

// ---------- 长期记忆（memory） ----------

// RememberPayload 是 remember 的 payload：写入一条长期记忆条目。
type RememberPayload struct {
	Kind           string `msgpack:"kind"`                      // 分类（自由文本：偏好 / 身份 / 知识 / 事件……）
	Content        string `msgpack:"content"`                   // 条目正文
	Priority       int    `msgpack:"priority,omitempty"`        // >0 = 核心记忆（每次对话都注入，不看相关性）
	ConversationID string `msgpack:"conversation_id,omitempty"` // 可空 = 全局条目
}

// RememberResult 是 remember 的结果：条目 id 与是否新建（重复内容幂等跳过）。
type RememberResult struct {
	ID      int64 `msgpack:"id"`
	Created bool  `msgpack:"created"`
}

// ForgetPayload 是 forget 的 payload：软删一条长期记忆（不在 provides 里——不给 LLM 删记忆的按钮）。
type ForgetPayload struct {
	ID int64 `msgpack:"id"`
}

// ForgetResult 是 forget 的结果。
type ForgetResult struct {
	ID int64 `msgpack:"id"`
}

// ---------- timer（计时器服务） ----------

// TimerSleepPayload 是 timer.sleep 的输入：多久之后把一条补报跳回调用方。
// Note 会原样带回到点补报里（给调用方自己看的备注，长度有上限，超出截断）。
type TimerSleepPayload struct {
	MS   int64  `msgpack:"ms"`             // 时长（毫秒）
	Note string `msgpack:"note,omitempty"` // 到点补报里原样带回的一句话
}

// TimerSleepResult 是 timer.sleep 的受理回执：计时已登记，到点由 report_result 补报送达。
// Phase 与补报（AsyncResultPayload.Phase）用同一词汇：受理回执就是这次计时的第一个阶段（queued）。
type TimerSleepResult struct {
	ItemID string `msgpack:"item_id"` // 受理句柄（补报原样带回）
	Phase  string `msgpack:"phase"`   // 阶段：queued
	FireAt string `msgpack:"fire_at"` // 预计到点时刻（RFC3339，本地时区；仅供人看）
}

// TimerCancelPayload 是 timer.cancel 的输入：撤掉一次已受理、尚未到点的计时。
type TimerCancelPayload struct {
	ItemID string `msgpack:"item_id"`
}

// TimerCancelResult 是 timer.cancel 的回执。
// Status：canceled（已撤销，并已发出一条 final 补报）/ unknown（没有这个句柄——早就到点、已被撤销或从不存在）。
type TimerCancelResult struct {
	ItemID string `msgpack:"item_id"`
	Status string `msgpack:"status"`
}

// TimerItem 是一条在计时器的对外描述（list 与门户快照共用）。
type TimerItem struct {
	ItemID string `msgpack:"item_id"`
	Caller string `msgpack:"caller"`  // 到点补报回谁
	MS     int64  `msgpack:"ms"`      // 时长（毫秒）
	FireAt string `msgpack:"fire_at"` // 到点时刻（RFC3339；门户据此倒计时）
	Note   string `msgpack:"note,omitempty"`
}

// TimerListResult 是 timer.list 的输出：当前所有在计时器。
// 门户冷启动时拉一次（之后的变动由 timer 主动推 timer_state 快照）。
type TimerListResult struct {
	Seq    int64       `msgpack:"seq"` // 快照序号（单调递增；门户据此丢弃乱序到达的旧快照）
	Timers []TimerItem `msgpack:"timers"`
}

// TimerStatePayload 是 timer 主动推给门户的快照（门户据此展示倒计时）。
// Boss 指明这批计时属于谁（信封 boss 原样带来）——门户据此认领该给哪个会话展示；
// 空 Boss 表示无主的计时，门户可自行归到默认会话。
// Seq 单调递增：骨架的 Fire 各自成 goroutine 投递、**到达顺序无保证**，
// 门户只接受比已见序号大的快照，旧的自然丢弃。
type TimerStatePayload struct {
	Boss   string      `msgpack:"boss,omitempty"`
	Seq    int64       `msgpack:"seq"`
	Timers []TimerItem `msgpack:"timers"` // 空数组 = 该 boss 的计时已清空
}

// ---------- 错误上报（服务 → 设置中心） ----------

// ReportPayload 是 report 的 payload：一条错误上报。
// 不含链路编号（当前没有链路记录者，记了也无处可查）；时间以中心收到为准。
type ReportPayload struct {
	Kind    string `msgpack:"kind,omitempty"` // 错误类别（上报方自定，如 fire / deliver / tool）
	Message string `msgpack:"message"`        // 错误简述
}

// ---------- 单步推理（llm 服务的裸入口） ----------

// ThinkPayload 是一次"给提示词、要一段文本"的单步推理请求。
//
// 这是**所有非任务型模型调用**的统一出口（复盘写经验、给经验簇起短标签…）：
// 别的服务不配自己的 LLM 客户端，一律 jump 到 llm 服务。它不带工具、不开任务、
// 不进黑板——就是一次补全，来去干净。
//
// Model 非空时覆盖默认模型（"最小不思考"用非推理模型就填这里）；
// MaxTokens 上限 4096（防止误填一个天文数字把账单打飞）。
type ThinkPayload struct {
	System    string `msgpack:"system,omitempty"`
	Prompt    string `msgpack:"prompt"`
	Model     string `msgpack:"model,omitempty"`
	MaxTokens int    `msgpack:"max_tokens,omitempty"`
}

// ThinkResult 是单步推理的结果。
type ThinkResult struct {
	Text string `msgpack:"text"`
}

// ---------- 经验网（记忆服务内的独立包 memnet） ----------

// NotePayload 写一条经验。整理时机由调用方决定（收尾后 / 定时 / 手动）。
type NotePayload struct {
	Content        string `msgpack:"content"`
	ConversationID string `msgpack:"conversation_id,omitempty"`
	TaskID         string `msgpack:"task_id,omitempty"`
	// Durability：core（不衰减）| durable（遗忘曲线，默认）| transient（不进网）。
	Durability string `msgpack:"durability,omitempty"`
}

// NoteResult 是写入结果：Duplicate = 同一件事又说了一遍（只加了命中次数）。
type NoteResult struct {
	ID        string   `msgpack:"id"`
	NodeIDs   []string `msgpack:"node_ids,omitempty"`
	Duplicate bool     `msgpack:"duplicate,omitempty"`
	Skipped   bool     `msgpack:"skipped,omitempty"`
}

// LineHeadsPayload 直接要线头（门户 / 调试用；recall 里是自动带的）。
type LineHeadsPayload struct {
	Query string `msgpack:"query"`
	Limit int    `msgpack:"limit,omitempty"`
}

// LineHeadView 是一条线头：一行代表句 + 条数 + 来自哪个任务。
type LineHeadView struct {
	NodeID  string   `msgpack:"node_id"`
	Text    string   `msgpack:"text"`
	Count   int      `msgpack:"count"`
	Score   float64  `msgpack:"score"`
	TaskID  string   `msgpack:"task_id,omitempty"`
	Members []string `msgpack:"members,omitempty"`
}

// LineHeadsResult 是线头列表。
type LineHeadsResult struct {
	Heads []LineHeadView `msgpack:"heads"`
}

// DigPayload 顺着线头往下扒：entry_id = 翻同簇兄弟，node_id = 展开整个大类。
type DigPayload struct {
	EntryID string `msgpack:"entry_id,omitempty"`
	NodeID  string `msgpack:"node_id,omitempty"`
	Limit   int    `msgpack:"limit,omitempty"`
}

// NoteView 是一条经验的只读视图。
type NoteView struct {
	ID         string `msgpack:"id"`
	Content    string `msgpack:"content"`
	TaskID     string `msgpack:"task_id,omitempty"`
	Source     string `msgpack:"source,omitempty"`
	Durability string `msgpack:"durability,omitempty"`
	Hits       int    `msgpack:"hits"`
}

// DigResult 是下探结果。
type DigResult struct {
	EntryID string     `msgpack:"entry_id,omitempty"`
	NodeID  string     `msgpack:"node_id,omitempty"`
	Content string     `msgpack:"content"`
	TaskID  string     `msgpack:"task_id,omitempty"`
	Notes   []NoteView `msgpack:"notes"`
	Total   int        `msgpack:"total"`
}

// PruneResult 是维护结果（软剪枝：只标状态，不删行）。
type PruneResult struct {
	Entries int `msgpack:"entries"`
	Links   int `msgpack:"links"`
}
